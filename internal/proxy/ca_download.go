package proxy

import (
	"net/http"
	"strconv"
	"time"
)

// serveCA publishes only the root certificate held by the active signer. It
// does not establish trust: clients must verify the fingerprint out of band.
func (p *Proxy) serveCA(w http.ResponseWriter, r *http.Request) {
	// The wire CONNECT validator inspects only the first request on a socket.
	// Never let a certificate request precede CONNECT on the same connection.
	w.Header().Set("Connection", "close")
	if p.cfg.IdleTimeout > 0 {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(p.cfg.IdleTimeout))
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		refuse(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		refuse(w, "request body denied", http.StatusBadRequest)
		return
	}
	if r.Header.Get("Upgrade") != "" || headerHasToken(r.Header, "Connection", "upgrade") {
		refuse(w, "upgrade denied", http.StatusBadRequest)
		return
	}
	if p.signer == nil {
		refuse(w, "CA unavailable", http.StatusServiceUnavailable)
		return
	}

	certificate := p.signer.CertificatePEM()
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Header().Set("Content-Disposition", `attachment; filename="ca.crt"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(certificate)))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(certificate)
	}
}
