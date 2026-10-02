package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
)

const maxEnvLineBytes = 64 * 1024

// readEnvFile reads an explicitly selected dotenv file. An empty path returns
// an empty map without searching for a file. It never changes the environment.
func readEnvFile(path string) (map[string]string, error) {
	values := make(map[string]string)
	if path == "" {
		return values, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read environment file: %w", err)
	}
	defer f.Close()

	allowed := make(map[string]struct{})
	for _, setting := range environmentSettings(&rawConfig{}) {
		allowed[environmentPrefix+setting.name] = struct{}{}
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), maxEnvLineBytes)
	line := 1
	for ; scanner.Scan(); line++ {
		name, value, skip, category := parseEnvLine(scanner.Text())
		if category != "" {
			return nil, fmt.Errorf("%w at line %d: %s", ErrInvalid, line, category)
		}
		if skip {
			continue
		}
		if _, ok := allowed[name]; !ok {
			return nil, fmt.Errorf("%w at line %d: unknown environment setting", ErrInvalid, line)
		}
		if _, duplicate := values[name]; duplicate {
			return nil, fmt.Errorf("%w at line %d: duplicate environment setting", ErrInvalid, line)
		}
		values[name] = value
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return nil, fmt.Errorf("%w at line %d: environment file line is too long", ErrInvalid, line)
		}
		return nil, fmt.Errorf("read environment file: %w", err)
	}
	return values, nil
}

func parseEnvLine(line string) (name, value string, skip bool, category string) {
	if strings.IndexByte(line, 0) >= 0 {
		return "", "", false, "NUL byte"
	}
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return "", "", true, ""
	}
	name, rest, found := strings.Cut(trimmed, "=")
	if !found || name == "" {
		return "", "", false, "expected KEY=value"
	}
	if strings.TrimSpace(name) != name || strings.ContainsAny(name, " \t") {
		return "", "", false, "malformed environment setting name"
	}
	// Whitespace outside quotes is insignificant. Quotes group literal bytes,
	// without escape processing, interpolation, or command execution.
	rest = strings.TrimSpace(rest)
	if len(rest) > 0 && (rest[0] == '\'' || rest[0] == '"') {
		quote := rest[0]
		end := strings.IndexByte(rest[1:], quote)
		if end < 0 {
			return "", "", false, "malformed quoted value"
		}
		end++
		value = rest[1:end]
		trailing := strings.TrimSpace(rest[end+1:])
		if trailing != "" && !strings.HasPrefix(trailing, "#") {
			return "", "", false, "trailing content after quoted value"
		}
		return name, value, false, ""
	}
	for i := 0; i < len(rest); i++ {
		if rest[i] == '#' && (i == 0 || rest[i-1] == ' ' || rest[i-1] == '\t') {
			rest = rest[:i]
			break
		}
	}
	return name, strings.TrimSpace(rest), false, ""
}
