package config

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"go.yaml.in/yaml/v3"
)

// YAML turns reader errors into parser messages, losing their identity. Keep the
// original read error so Load can distinguish I/O failures from invalid YAML.
type readErrorTracker struct {
	io.Reader
	err error
}

func (r *readErrorTracker) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err != nil && err != io.EOF && r.err == nil {
		r.err = err
	}
	return n, err
}

// Reasons passed here are fixed messages, never configuration values.
func invalidNode(node *yaml.Node, reason string) error {
	return fmt.Errorf("%w at line %d: %s", ErrInvalid, node.Line, reason)
}

// YAML errors can contain values, keys, tags, or anchor names supplied by the
// operator. Preserve only a numeric line and a fixed category, not their text.
func safeDecodeError(err error) error {
	if errors.Is(err, ErrInvalid) {
		return err // Our custom scalar decoders already produce sanitized errors.
	}
	if errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: expected a YAML document", ErrInvalid)
	}
	var line int
	reason := "malformed YAML"
	var typeError *yaml.TypeError
	if errors.As(err, &typeError) && len(typeError.Errors) > 0 {
		message := typeError.Errors[0]
		fmt.Sscanf(message, "line %d:", &line)
		reason = "incorrect YAML value type"
		switch {
		case strings.Contains(message, " not found in type "):
			reason = "unknown field"
		case strings.Contains(message, " already defined at line "):
			reason = "duplicate mapping key"
		}
	} else {
		fmt.Sscanf(err.Error(), "yaml: line %d:", &line)
	}
	if line > 0 {
		return fmt.Errorf("%w at line %d: %s", ErrInvalid, line, reason)
	}
	return fmt.Errorf("%w: %s", ErrInvalid, reason)
}
