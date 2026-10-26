package hostport

import (
	"errors"
	"net"
	"strconv"
	"strings"
)

// ErrInvalid is returned for any text that is not a valid authority.
var ErrInvalid = errors.New("invalid authority")

// HostPort is a canonical host and port pair. Construction always
// canonicalizes, so values are safe to compare with == and to use as map keys.
type HostPort struct {
	host string
	port string
}

// Parse parses text as a host or host:port, using defaultPort when the port is
// omitted (an empty defaultPort requires one). IPv6 hosts need brackets with a
// port. Ports must be 1–65535, and hosts follow [Canonicalize]. Failures return
// [ErrInvalid]. See the package documentation for rejected input.
func Parse(text, defaultPort string) (HostPort, error) {
	if text == "" || text != strings.TrimSpace(text) || strings.ContainsAny(text, "@/?#%*") {
		return HostPort{}, ErrInvalid
	}

	host, port, err := split(text)
	if err != nil {
		return HostPort{}, ErrInvalid
	}

	if port == "" {
		port = defaultPort
	}
	if port == "" || !validPort(port) {
		return HostPort{}, ErrInvalid
	}
	portNumber, _ := strconv.Atoi(port)
	port = strconv.Itoa(portNumber)

	host, err = Canonicalize(host)
	if err != nil {
		return HostPort{}, err
	}

	return HostPort{host: host, port: port}, nil
}

// Canonicalize returns the canonical form of a bare host: lowercased, with a
// single trailing dot removed, IP literals rewritten to their canonical text via
// net.ParseIP, and hostnames validated by the same label rules Parse enforces. It
// returns ErrInvalid for an empty host, an IPv4 written in a non-canonical form,
// or a hostname with an invalid label. It does not accept or strip a port.
func Canonicalize(host string) (string, error) {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "" || strings.Contains(host, ":") && net.ParseIP(host) == nil {
		return "", ErrInvalid
	}

	if ip := net.ParseIP(host); ip != nil {
		return ip.String(), nil
	}
	if looksLikeIPv4(host) || !validHostname(host) {
		return "", ErrInvalid
	}

	return host, nil
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

// looksLikeIPv4 reports whether host has the dotted-decimal shape of an IPv4
// address. Canonicalize calls it only after net.ParseIP has failed, so a match
// is a non-canonical form such as "010.0.0.1", which some resolvers would read
// as octal, so it is rejected rather than reinterpreted.
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

// validHostname applies DNS label rules to a lowercased host: at most 253
// characters, with labels of 1–63 characters from a–z, 0–9, and '-' that do
// not start or end with '-'.
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

// Host returns the canonical host, without brackets for IPv6.
func (h HostPort) Host() string { return h.host }

// Port returns the decimal port without leading zeros.
func (h HostPort) Port() string { return h.port }

// String returns the authority, omitting the port when it is 443 and
// bracketing IPv6 hosts.
func (h HostPort) String() string {
	if h.port == "443" {
		if strings.Contains(h.host, ":") {
			return "[" + h.host + "]"
		}
		return h.host
	}

	return net.JoinHostPort(h.host, h.port)
}
