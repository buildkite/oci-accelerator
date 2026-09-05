// Package authority parses and canonicalizes network authorities.
package authority

import (
	"errors"
	"net"
	"strconv"
	"strings"
)

var ErrInvalid = errors.New("invalid authority")

// Authority is a canonical host and port pair.
type Authority struct {
	host string
	port string
}

// Parse parses text as a host or host:port. If the port is omitted, defaultPort
// is used; an empty defaultPort therefore requires an explicit port.
func Parse(text, defaultPort string) (Authority, error) {
	if text == "" || text != strings.TrimSpace(text) || strings.ContainsAny(text, "@/?#%*") {
		return Authority{}, ErrInvalid
	}

	host, port, err := split(text)
	if err != nil {
		return Authority{}, ErrInvalid
	}
	if port == "" {
		port = defaultPort
	}
	if port == "" || !validPort(port) {
		return Authority{}, ErrInvalid
	}
	portNumber, _ := strconv.Atoi(port)
	port = strconv.Itoa(portNumber)

	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "" || strings.Contains(host, ":") && net.ParseIP(host) == nil {
		return Authority{}, ErrInvalid
	}
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	} else if looksLikeIPv4(host) || !validHostname(host) {
		return Authority{}, ErrInvalid
	}

	return Authority{host: host, port: port}, nil
}

func split(text string) (string, string, error) {
	if strings.HasSuffix(text, ":") && !strings.HasSuffix(text, "::") {
		return "", "", ErrInvalid
	}
	if strings.HasPrefix(text, "[") {
		end := strings.IndexByte(text, ']')
		if end < 0 || net.ParseIP(text[1:end]) == nil || !strings.Contains(text[1:end], ":") {
			return "", "", ErrInvalid
		}
		if strings.HasSuffix(text, "]") {
			return strings.TrimSuffix(strings.TrimPrefix(text, "["), "]"), "", nil
		}
		return net.SplitHostPort(text)
	}
	if strings.Count(text, ":") > 1 { // bare IPv6 without a port
		return text, "", nil
	}
	if strings.Contains(text, ":") {
		return net.SplitHostPort(text)
	}
	return text, "", nil
}

func looksLikeIPv4(host string) bool {
	if strings.Count(host, ".") != 3 {
		return false
	}
	for _, r := range host {
		if r != '.' && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func validPort(port string) bool {
	n, err := strconv.ParseUint(port, 10, 16)
	return err == nil && n > 0
}

func validHostname(host string) bool {
	if len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	return true
}

func (a Authority) Host() string { return a.host }
func (a Authority) Port() string { return a.port }

func (a Authority) String() string {
	if a.port == "443" {
		if strings.Contains(a.host, ":") {
			return "[" + a.host + "]"
		}
		return a.host
	}
	return net.JoinHostPort(a.host, a.port)
}
