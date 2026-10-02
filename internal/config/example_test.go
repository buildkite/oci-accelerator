package config

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestEnvironmentExampleContainsEverySetting(t *testing.T) {
	values, err := readEnvFile(filepath.Join("..", "..", ".env.example"))
	if err != nil {
		t.Fatal(err)
	}
	raw := defaults()
	settings := environmentSettings(&raw)
	if len(settings) != 22 {
		t.Fatalf("environment settings = %d, want 22", len(settings))
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
