package peering

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/buildkite/oci-accelerator/internal/cache"
)

var errFetchUnavailable = errors.New("peer cache entry unavailable")

// Transfer serves local entries and fetches advertised entries over fleet TLS.
// It never receives registry credentials or makes authorization decisions.
// The caller must authorize the request and verify fetched bytes before commit.
type Transfer struct {
	book         *Book
	availability *Availability
	client       *http.Client
	handler      http.Handler
	ctx          context.Context
	cancel       context.CancelFunc
	timeout      time.Duration
	slots        chan struct{}
}

// NewTransfer creates a separate bulk-data client. Discovery and availability
// retain their short request deadlines and independent connection pools.
func NewTransfer(ctx context.Context, book *Book, availability *Availability, store *cache.Store, tlsConfig *tls.Config, timeout time.Duration) *Transfer {
	ctx, cancel := context.WithCancel(ctx)
	client := NewClient(tlsConfig)
	client.Timeout = 0 // Fetch owns separate setup and body deadlines.
	transport := client.Transport.(*http.Transport)
	transport.DisableKeepAlives = true // Do not occupy the shared listener with idle bulk connections.
	transport.MaxConnsPerHost = maxPeerTransfers
	return &Transfer{
		book: book, availability: availability, client: client,
		handler: newFetchHandler(ctx, book.Snapshot().NodeID, store, timeout),
		ctx:     ctx, cancel: cancel, timeout: timeout, slots: make(chan struct{}, maxPeerTransfers),
	}
}

// Handler returns the local-only entry endpoint for the mutual-TLS listener.
func (t *Transfer) Handler() http.Handler { return t.handler }

// Close cancels active outbound streams. Their owners must still close bodies.
func (t *Transfer) Close() {
	t.cancel()
	t.client.CloseIdleConnections()
}

// Fetch tries at most two resolved owners with a shared five-second setup
// budget. A successful caller owns the returned body and must close it. Once
// accepted, a stream never retries or changes sources, even on a read failure.
func (t *Transfer) Fetch(ctx context.Context, meta cache.Metadata) (io.ReadCloser, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	peers := t.candidates(meta.Key)
	if len(peers) == 0 {
		return nil, errFetchUnavailable
	}
	select {
	case t.slots <- struct{}{}:
	default:
		return nil, errFetchUnavailable
	}

	// Do not inherit the proxy's upstream httptrace or request values. Only
	// cancellation crosses this boundary, so peer I/O cannot count as origin I/O.
	transferCtx, cancel := context.WithTimeout(t.ctx, t.timeout)
	stopCaller := context.AfterFunc(ctx, cancel)
	release := sync.OnceFunc(func() {
		stopCaller()
		cancel()
		<-t.slots
	})
	setup := time.AfterFunc(5*time.Second, cancel)
	defer setup.Stop()
	for _, peer := range peers[:min(2, len(peers))] {
		body, err := t.fetch(transferCtx, peer, meta)
		if err == nil {
			if !setup.Stop() || transferCtx.Err() != nil || ctx.Err() != nil {
				body.Close()
				break
			}
			return &fetchBody{ReadCloser: body, release: release}, nil
		}
		if transferCtx.Err() != nil {
			break
		}
	}
	release()
	return nil, errFetchUnavailable
}

func (t *Transfer) candidates(key string) []destination {
	owners := t.availability.Owners(key)
	book := t.book.Snapshot()
	addresses := make(map[string]string, len(book.Peers))
	for address, id := range book.Peers {
		addresses[id] = address
	}
	var peers []destination
	for _, owner := range owners {
		if address := addresses[owner]; owner != book.NodeID && address != "" {
			id, _ := parseNodeID(owner)
			peers = append(peers, destination{id: id, address: address})
		}
	}
	rand.Shuffle(len(peers), func(i, j int) { peers[i], peers[j] = peers[j], peers[i] })
	return peers
}

func (t *Transfer) fetch(ctx context.Context, peer destination, meta cache.Metadata) (io.ReadCloser, error) {
	wire := fetchRequest{RecipientNodeID: peer.id.String(), Key: meta.Key, Digest: meta.Digest, Size: meta.Size, MediaType: meta.MediaType}
	if wire.validate() != nil {
		return nil, errFetchUnavailable
	}
	encoded, err := json.Marshal(wire)
	if err != nil || len(encoded) > maxFetchRequestBytes {
		return nil, errFetchUnavailable
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+peer.address+fetchPath, bytes.NewReader(encoded))
	if err != nil {
		return nil, errFetchUnavailable
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := t.client.Do(request)
	if err != nil {
		return nil, errFetchUnavailable
	}
	if response.StatusCode != http.StatusOK || response.ContentLength != meta.Size || response.Uncompressed || len(response.Trailer) != 0 || len(response.TransferEncoding) != 0 ||
		!exactHeader(response.Header, "Content-Type", meta.MediaType) || !exactHeader(response.Header, "Docker-Content-Digest", meta.Digest) || !exactHeader(response.Header, "X-OCI-Node-ID", peer.id.String()) ||
		len(response.Header.Values("Content-Encoding")) != 0 || len(response.Header.Values("Content-Range")) != 0 {
		response.Body.Close()
		return nil, errFetchUnavailable
	}
	return response.Body, nil
}

func exactHeader(header http.Header, name, value string) bool {
	values := header.Values(name)
	return len(values) == 1 && values[0] == value
}

type fetchBody struct {
	io.ReadCloser
	release func()
}

func (b *fetchBody) Close() error {
	err := b.ReadCloser.Close()
	b.release()
	return err
}
