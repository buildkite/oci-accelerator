package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/buildkite/go-crew-kit/envconfig"
)

func TestEnvironmentExampleContainsEverySetting(t *testing.T) {
	path := filepath.Join("..", "..", ".env.example")
	names := exampleSettingNames(t, path)
	raw := defaults()
	settings := environmentSettings(&raw)
	if len(settings) != 31 {
		t.Fatalf("environment settings = %d, want 31", len(settings))
	}

	for _, setting := range settings {
		name := environmentPrefix + setting.Name
		if !names[name] {
			t.Errorf(".env.example lacks %s", name)
		}
		delete(names, name)
	}

	if len(names) != 0 {
		t.Errorf(".env.example has unknown settings: %v", names)
	}
	if _, err := load(path, environmentLookup(nil)); err != nil {
		t.Fatalf(".env.example does not validate: %v", err)
	}
}

func TestContainerExampleMatchesImageDefaults(t *testing.T) {
	image := dockerfileEnvironment(t)
	if len(image) != 4 {
		t.Fatalf("Dockerfile environment = %v, want the four image defaults", image)
	}

	path := filepath.Join("..", "..", ".env.container-example")
	assertPortableDockerEnvFile(t, path)
	names := exampleSettingNames(t, path)
	for name := range image {
		if !names[name] {
			t.Errorf(".env.container-example lacks %s", name)
		}
	}
	if len(names) != len(image)+1 || !names[environmentPrefix+"REGISTRIES"] {
		t.Errorf(".env.container-example = %v, want only REGISTRIES and image defaults", names)
	}

	c, err := load(path, environmentLookup(nil))
	if err != nil {
		t.Fatalf(".env.container-example does not validate: %v", err)
	}
	assertEqual(t, "Peer.HintRefreshTimeout", c.Peer.HintRefreshTimeout, DefaultPeerHintRefreshTimeout)
	assertEqual(t, "Peer.HintRefreshInterval", c.Peer.HintRefreshInterval, DefaultPeerHintRefreshInterval)
	assertEqual(t, "Peer.HintFullRefreshInterval", c.Peer.HintFullRefreshInterval, DefaultPeerHintFullRefreshInterval)

	image[environmentPrefix+"REGISTRIES"] = "example.com"
	want, err := load("", func(name string) (string, bool) { value, ok := image[name]; return value, ok })
	if err != nil {
		t.Fatal(err)
	}
	want.Registries = c.Registries
	if len(c.Registries) == 0 || !reflect.DeepEqual(c, want) {
		t.Errorf("container example = %+v, want image defaults %+v", c, want)
	}
}

func TestEnvironmentExampleMatchesRawDefaults(t *testing.T) {
	got := defaults()
	loader := envconfig.Loader{Prefix: environmentPrefix, LookupEnv: environmentLookup(nil)}
	if err := loader.Load(filepath.Join("..", "..", ".env.example"), environmentSettings(&got)); err != nil {
		t.Fatal(err)
	}

	assertEqual(t, "example peer state path", got.Peer.StateDir, "./data/peers")
	got.Registries = nil
	got.CADir = ""
	got.Cache.Dir = ""
	got.Peer.StateDir = ""

	want := defaults()
	if !reflect.DeepEqual(got, want) {
		t.Errorf(".env.example defaults = %+v, want %+v", got, want)
	}
}

// exampleSettingNames checks fixture completeness without parsing values.
// Tests load the examples separately to check their syntax and validation.
func exampleSettingNames(t *testing.T, path string) map[string]bool {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	names := make(map[string]bool)
	for _, line := range strings.Split(string(contents), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		name, _, _ := strings.Cut(line, "=")
		names[name] = true
	}

	return names
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
