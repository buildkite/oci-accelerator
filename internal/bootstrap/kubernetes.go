package bootstrap

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

type apiClient struct {
	http  *http.Client
	base  string
	token string
}

type resource struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   metadata          `json:"metadata"`
	Immutable  bool              `json:"immutable"`
	Data       map[string]string `json:"data"`
	BinaryData map[string]string `json:"binaryData,omitempty"`
	Type       string            `json:"type,omitempty"`
}

type metadata struct {
	Name string `json:"name"`
}

func inClusterClient() (*apiClient, error) {
	const account = "/var/run/secrets/kubernetes.io/serviceaccount/"
	namespace, err := os.ReadFile(account + "namespace")
	if err != nil || !validName(strings.TrimSpace(string(namespace))) {
		return nil, errors.New("cannot read service account namespace")
	}
	token, err := os.ReadFile(account + "token")
	if err != nil || len(bytes.TrimSpace(token)) == 0 {
		return nil, errors.New("cannot read service account token")
	}
	cert, err := os.ReadFile(account + "ca.crt")
	if err != nil {
		return nil, errors.New("cannot read Kubernetes API trust roots")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(cert) {
		return nil, errors.New("invalid Kubernetes API trust roots")
	}
	transport := &http.Transport{
		// Do not inherit the Docker proxy environment or follow API redirects.
		TLSClientConfig:       &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
	}
	return &apiClient{
		http:  &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: refuseRedirect},
		base:  "https://kubernetes.default.svc/api/v1/namespaces/" + strings.TrimSpace(string(namespace)),
		token: strings.TrimSpace(string(token)),
	}, nil
}

func (c *apiClient) get(ctx context.Context, collection, name string) (*resource, error) {
	item, status, err := c.request(ctx, http.MethodGet, collection+"/"+name, nil)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, nil
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("Kubernetes API GET failed (HTTP %d)", status)
	}
	return item, nil
}

func (c *apiClient) create(ctx context.Context, collection string, item *resource) error {
	_, status, err := c.request(ctx, http.MethodPost, collection, item)
	if err != nil {
		return err
	}
	if status == http.StatusConflict {
		return nil
	}
	if status != http.StatusCreated {
		return fmt.Errorf("Kubernetes API create failed (HTTP %d)", status)
	}
	return nil
}

func (c *apiClient) request(ctx context.Context, method, path string, item *resource) (*resource, int, error) {
	var body []byte
	if item != nil {
		var err error
		body, err = json.Marshal(item)
		if err != nil {
			return nil, 0, errors.New("cannot encode Kubernetes resource")
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+"/"+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, errors.New("cannot construct Kubernetes API request")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, errors.New("Kubernetes API request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		// API diagnostics can include submitted Secret data; never echo them.
		return nil, resp.StatusCode, nil
	}
	const limit = 1 << 20
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || len(raw) > limit {
		return nil, 0, errors.New("cannot read bounded Kubernetes API response")
	}
	var result resource
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, 0, errors.New("invalid Kubernetes API response")
	}
	return &result, resp.StatusCode, nil
}

func refuseRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
