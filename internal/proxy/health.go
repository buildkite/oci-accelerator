package proxy

// Ready reports whether the initialized proxy is serving and not draining.
// It deliberately performs no registry, DNS or disk I/O: an upstream outage or
// cache admission failure must not turn every replica unready or cause restarts.
func (p *Proxy) Ready() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.listener != nil && !p.closing && p.signer != nil
}
