package config

import (
	"strconv"
	"time"

	"go.yaml.in/yaml/v3"
)

type duration time.Duration

// strictBool requires an actual YAML boolean, not YAML 1.1 string coercions.
type strictBool bool

func (b *strictBool) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode && node.Tag == "!!null" {
		return nil
	}
	if node.Kind != yaml.ScalarNode || node.Tag != "!!bool" {
		return invalidNode(node, "must be a boolean")
	}
	value, err := strconv.ParseBool(node.Value)
	if err != nil {
		return invalidNode(node, "must be a boolean")
	}
	*b = strictBool(value)
	return nil
}

// strictString prevents YAML from coercing other scalar types into strings.
// Null scalar fields retain their defaults, and pointer list elements remain nil.
type strictString string

func (s *strictString) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode && node.Tag == "!!null" {
		return nil
	}
	if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
		return invalidNode(node, "must be a string")
	}
	*s = strictString(node.Value)
	return nil
}

// strictInt64 and strictInt reject any scalar that is not a decimal YAML integer,
// mirroring duration's tag guard so that a float, quoted string, or boolean cannot
// silently truncate into a numeric limit. A null scalar preserves the default. On
// the 64-bit build target, int spans the full int64 range parseStrictInt validates,
// so no in-range value is narrowed when converting strictInt. 32-bit builds, where
// a value above MaxInt could wrap, are out of scope.
type strictInt64 int64
type strictInt int

// parseStrictInt decodes node as a decimal YAML integer. It returns (0, true) for a
// null node so callers can preserve their default, and ErrInvalid for any non-!!int
// tag or a value that decimal strconv.ParseInt cannot represent (including overflow
// and non-decimal integer forms such as 0x, 0o, 0-prefixed octal, and underscores).
func parseStrictInt(node *yaml.Node) (int64, bool, error) {
	if node.Tag == "!!null" {
		return 0, true, nil
	}
	if node.Tag != "!!int" {
		return 0, false, invalidNode(node, "must be a decimal integer")
	}
	if !hasNoLeadingZero(node.Value) {
		return 0, false, invalidNode(node, "must be a decimal integer without leading zeros")
	}
	v, err := strconv.ParseInt(node.Value, 10, 64)
	if err != nil {
		return 0, false, invalidNode(node, "must be a decimal integer within the int64 range")
	}
	return v, false, nil
}

// hasNoLeadingZero rejects redundant leading zeros after an optional sign.
// strconv.ParseInt separately validates the digits and range.
func hasNoLeadingZero(s string) bool {
	if len(s) > 0 && (s[0] == '+' || s[0] == '-') {
		s = s[1:]
	}
	return len(s) == 1 || (len(s) > 1 && s[0] != '0')
}

func (n *strictInt64) UnmarshalYAML(node *yaml.Node) error {
	v, isNull, err := parseStrictInt(node)
	if err != nil {
		return err
	}
	if isNull {
		return nil
	}
	*n = strictInt64(v)
	return nil
}

func (n *strictInt) UnmarshalYAML(node *yaml.Node) error {
	v, isNull, err := parseStrictInt(node)
	if err != nil {
		return err
	}
	if isNull {
		return nil
	}
	*n = strictInt(v)
	return nil
}

func (d *duration) UnmarshalYAML(node *yaml.Node) error {
	if node.Tag == "!!null" {
		return nil
	}
	if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
		return invalidNode(node, "must be a duration string")
	}
	v, err := time.ParseDuration(node.Value)
	if err != nil {
		return invalidNode(node, "must be a duration string")
	}
	*d = duration(v)
	return nil
}
