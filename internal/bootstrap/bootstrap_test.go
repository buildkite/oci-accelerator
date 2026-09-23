package bootstrap

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

func TestProvisionPersistsIdentityAndRecoversPartialPublication(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	f := newFixture(t)
	f.failPublic = true
	if err := provision(context.Background(), f.client, "identity", "trust"); err == nil {
		t.Fatal("expected ConfigMap publication failure")
	}
	secret := f.items["secrets/identity"]
	if secret == nil || !secret.Immutable || len(secret.Data) != 2 {
		t.Fatal("missing immutable CA Secret")
	}
	originalKey := secret.Data["ca.key"]
	certPEM, err := base64.StdEncoding.DecodeString(secret.Data["ca.crt"])
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("missing PEM certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil || !cert.IsCA || cert.CheckSignatureFrom(cert) != nil {
		t.Fatal("not a valid self-signed CA")
	}
	f.failPublic = false
	for range 2 {
		if err := provision(context.Background(), f.client, "identity", "trust"); err != nil {
			t.Fatal(err)
		}
	}
	public := f.items["configmaps/trust"]
	if !public.Immutable || len(public.Data) != 1 || public.Data["ca.crt"] != string(certPEM) {
		t.Fatal("public trust differs from persisted CA")
	}
	if f.items["secrets/identity"].Data["ca.key"] != originalKey || f.secretCreates != 1 || f.publicCreates != 2 {
		t.Fatal("retry replaced identity or republished existing resources")
	}
	entries, err := os.ReadDir(os.Getenv("TMPDIR"))
	if err != nil || len(entries) != 0 {
		t.Fatal("private CA temporary files were not cleaned up")
	}
}

func TestProvisionAdoptsConcurrentWinner(t *testing.T) {
	f := newFixture(t)
	winner, err := caData(nil)
	if err != nil {
		t.Fatal(err)
	}
	f.competingSecret = &resource{Immutable: true, Data: winner}
	if err := provision(context.Background(), f.client, "identity", "trust"); err != nil {
		t.Fatal(err)
	}
	cert, _ := base64.StdEncoding.DecodeString(winner["ca.crt"])
	if f.items["configmaps/trust"].Data["ca.crt"] != string(cert) {
		t.Fatal("published losing identity instead of concurrent winner")
	}
}

func TestProvisionFailsClosedWithoutReplacingState(t *testing.T) {
	for _, scenario := range []string{"lost-secret", "bad-key", "mismatched-public", "mutable-secret", "extra-secret-field", "extra-public-binary"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFixture(t)
			if err := provision(context.Background(), f.client, "identity", "trust"); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "lost-secret":
				delete(f.items, "secrets/identity")
			case "bad-key":
				f.items["secrets/identity"].Data["ca.key"] = base64.StdEncoding.EncodeToString([]byte("private-sentinel"))
			case "mismatched-public":
				f.items["configmaps/trust"].Data["ca.crt"] = "unrelated CA"
			case "mutable-secret":
				f.items["secrets/identity"].Immutable = false
			case "extra-secret-field":
				f.items["secrets/identity"].Data["password"] = "private-sentinel"
			case "extra-public-binary":
				f.items["configmaps/trust"].BinaryData = map[string]string{"ca.key": base64.StdEncoding.EncodeToString([]byte("private-sentinel"))}
			}
			err := provision(context.Background(), f.client, "identity", "trust")
			if err == nil || strings.Contains(err.Error(), "private-sentinel") {
				t.Fatalf("expected safe failure, got %v", err)
			}
			if f.secretCreates != 1 || f.publicCreates != 1 {
				t.Fatal("invalid state caused resource writes")
			}
		})
	}
}

func TestAPIRefusesRedirectsAndRedactsFailures(t *testing.T) {
	var destinationRequests int
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		destinationRequests++
		w.WriteHeader(http.StatusOK)
	}))
	defer destination.Close()
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusForbidden, http.StatusInternalServerError} {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", destination.URL)
			w.WriteHeader(status)
			_, _ = w.Write([]byte("secret-sentinel"))
		}))
		client := &apiClient{http: server.Client(), base: server.URL, token: "secret-sentinel"}
		client.http.CheckRedirect = refuseRedirect
		_, err := client.get(context.Background(), "secrets", "identity")
		server.Close()
		if err == nil || strings.Contains(err.Error(), "secret-sentinel") {
			t.Fatalf("expected redacted error, got %v", err)
		}
	}
	if destinationRequests != 0 {
		t.Fatal("followed API redirect")
	}
}

func TestAPICancellationAndResponseLimit(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := provision(ctx, f.client, "identity", "trust"); err == nil {
		t.Fatal("canceled provisioning succeeded")
	}
	if f.secretCreates != 0 {
		t.Fatal("canceled provisioning created a Secret")
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat(" ", (1<<20)+1)))
	}))
	defer server.Close()
	client := &apiClient{http: server.Client(), base: server.URL}
	if _, err := client.get(context.Background(), "secrets", "identity"); err == nil {
		t.Fatal("unbounded API response accepted")
	}
}

type fixture struct {
	client                       *apiClient
	mu                           sync.Mutex
	items                        map[string]*resource
	secretCreates, publicCreates int
	failPublic                   bool
	competingSecret              *resource
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{items: make(map[string]*resource)}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Error("missing API authorization")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/")
		switch r.Method {
		case http.MethodGet:
			item := f.items[path]
			if item == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(item)
		case http.MethodPost:
			var item resource
			if err := json.NewDecoder(r.Body).Decode(&item); err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			key := path + "/" + item.Metadata.Name
			if path == "secrets" {
				f.secretCreates++
				if f.competingSecret != nil {
					f.items[key] = f.competingSecret
					f.competingSecret = nil
				}
			} else if path == "configmaps" {
				f.publicCreates++
				if f.failPublic {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
			} else {
				t.Error("unexpected collection")
			}
			if f.items[key] != nil {
				w.WriteHeader(http.StatusConflict)
				return
			}
			f.items[key] = &item
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(&item)
		default:
			t.Error("bootstrap attempted a method other than GET or POST")
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(server.Close)
	f.client = &apiClient{http: server.Client(), base: server.URL, token: "fixture-token"}
	f.client.http.CheckRedirect = refuseRedirect
	return f
}
