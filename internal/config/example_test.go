package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestEnvironmentExampleContainsEverySetting(t *testing.T) {
	values, err := readEnvFile(filepath.Join("..", "..", ".env.example"))
	if err != nil {
		t.Fatal(err)
	}
	raw := defaults()
	settings := environmentSettings(&raw)
	if len(settings) != 30 {
		t.Fatalf("environment settings = %d, want 30", len(settings))
	}
	for _, setting := range settings {
		name := "OCI_ACCELERATOR_" + setting.name
		if _, ok := values[name]; !ok {
			t.Errorf(".env.example lacks %s", name)
		}
		delete(values, name)
	}
	if len(values) != 0 {
		t.Errorf(".env.example has unknown settings: %v", values)
	}
}

func TestContainerExampleMatchesImageDefaults(t *testing.T) {
	image := dockerfileEnvironment(t)
	if len(image) != 4 {
		t.Fatalf("Dockerfile environment = %v, want the four image defaults", image)
	}
	path := filepath.Join("..", "..", ".env.container-example")
	assertPortableDockerEnvFile(t, path)
	example, err := readEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range image {
		if example[name] != value {
			t.Errorf(".env.container-example %s = %q, want image default %q", name, example[name], value)
		}
	}
	if len(example) != len(image)+1 || example[environmentPrefix+"REGISTRIES"] == "" {
		t.Errorf(".env.container-example = %v, want only REGISTRIES and image defaults", example)
	}
	c, err := load(path, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatalf(".env.container-example does not validate: %v", err)
	}
	assertEqual(t, "Peer.HintRefreshTimeout", c.Peer.HintRefreshTimeout, DefaultPeerHintRefreshTimeout)
	assertEqual(t, "Peer.HintRefreshInterval", c.Peer.HintRefreshInterval, DefaultPeerHintRefreshInterval)
	assertEqual(t, "Peer.HintFullRefreshInterval", c.Peer.HintFullRefreshInterval, DefaultPeerHintFullRefreshInterval)
	image[environmentPrefix+"REGISTRIES"] = "example.com"
	if _, err := load("", func(name string) (string, bool) { value, ok := image[name]; return value, ok }); err != nil {
		t.Errorf("image defaults with only REGISTRIES do not validate: %v", err)
	}
}

func TestEnvironmentExampleMatchesRawDefaults(t *testing.T) {
	values, err := readEnvFile(filepath.Join("..", "..", ".env.example"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"OCI_ACCELERATOR_REGISTRIES", "OCI_ACCELERATOR_CA_DIR", "OCI_ACCELERATOR_CACHE_DIR"} {
		delete(values, name)
	}
	want := defaults()
	got := defaults()
	if err := applyEnvironment(&got, func(name string) (string, bool) { value, ok := values[name]; return value, ok }); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf(".env.example defaults = %+v, want %+v", got, want)
	}
}

// dockerfileEnvironment returns the OCI_ACCELERATOR_* values that the final
// stage of the root Dockerfile sets with single-variable ENV lines.
func dockerfileEnvironment(t *testing.T) map[string]string {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]string
	for _, line := range strings.Split(string(contents), "\n") {
		if strings.HasPrefix(line, "FROM ") {
			values = make(map[string]string)
			continue
		}
		assignment, ok := strings.CutPrefix(line, "ENV ")
		if !ok || !strings.HasPrefix(assignment, environmentPrefix) {
			continue
		}
		name, value, ok := strings.Cut(assignment, "=")
		if !ok || strings.ContainsAny(value, " \t\"'\\$") {
			t.Fatalf("Dockerfile ENV line must be a single unquoted literal KEY=value: %q", line)
		}
		values[name] = value
	}
	return values
}

// assertPortableDockerEnvFile requires only full-line comments, blank lines,
// and literal KEY=value lines, which Docker, Compose, and the proxy read alike.
func assertPortableDockerEnvFile(t *testing.T, path string) {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for i, line := range strings.Split(string(contents), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		_, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(line) != line || strings.ContainsAny(value, " \t#\"'\\$") {
			t.Errorf("%s line %d is not a portable literal KEY=value: %q", path, i+1, line)
		}
	}
}
