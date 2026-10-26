package peering

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"slices"
	"sync"

	"github.com/buildkite/oci-accelerator/internal/cache"
)

// Fetch tries one uniformly random live provider. The client must bound the
// whole transfer, not just response headers. The caller owns the returned body
// and must close it, releasing an outbound slot. Busy or absent providers fail
// immediately so the caller can use its already-authorized origin stream.
func (p *ProviderHints) Fetch(ctx context.Context, client *http.Client, meta cache.Metadata) (io.ReadCloser, error) {
	key, ok := parseCacheKey(meta.Key)
	if !ok || ctx.Err() != nil {
		return nil, errUnknownPeer
	}

	select {
	case p.transfers <- struct{}{}:
	default:
		return nil, errUnknownPeer
	}
	release := sync.OnceFunc(func() { <-p.transfers })

	peers := p.book.Snapshot().Peers
	p.mu.Lock()
	claims := slices.Clone(p.index.byKey[key])
	p.mu.Unlock()

	claims = slices.DeleteFunc(claims, func(claim providerClaim) bool {
		return claim.id == p.self || addressForID(peers, claim.id.String()) == ""
	})
	if len(claims) == 0 {
		release()
		return nil, errUnknownPeer
	}

	claim := claims[rand.IntN(len(claims))]
	// Resolve again at dispatch, rather than retaining an address in the hint.
	address := addressForID(p.book.Snapshot().Peers, claim.id.String())
	if address == "" {
		release()
		return nil, errUnknownPeer
	}

	body, err := FetchEntry(ctx, client, address, claim.id.String(), meta)
	if err != nil {
		release()
		if errors.Is(err, ErrEntryMiss) {
			p.mu.Lock()
			p.index.forget(key, claim)
			p.mu.Unlock()
		}
		return nil, err
	}
	return &providerBody{ReadCloser: body, release: release}, nil
}

type providerBody struct {
	io.ReadCloser
	release func()
}

func (b *providerBody) Close() error {
	err := b.ReadCloser.Close()
	b.release()
	return err
}
