package config

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
)

func TestReadErrorTracker(t *testing.T) {
	wantError := errors.New("read failed")
	for _, test := range []struct {
		name   string
		reader io.Reader
		data   string
		err    error
	}{
		{"empty EOF", strings.NewReader(""), "", nil},
		{"EOF after data", strings.NewReader("data"), "data", nil},
		{"read failure", iotest.ErrReader(wantError), "", wantError},
		{"failure after data", io.MultiReader(strings.NewReader("data"), iotest.ErrReader(wantError)), "data", wantError},
		{"data with failure", iotest.DataErrReader(io.MultiReader(strings.NewReader("data"), iotest.ErrReader(wantError))), "data", wantError},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := &readErrorTracker{Reader: test.reader}
			data, err := io.ReadAll(reader)
			assertEqual(t, "data", string(data), test.data)
			if !errors.Is(err, test.err) || !errors.Is(reader.err, test.err) {
				t.Errorf("read error = %v, tracked error = %v, want %v", err, reader.err, test.err)
			}
		})
	}
}

func TestLoadFileErrors(t *testing.T) {
	for _, test := range []struct{ name, path, operation string }{
		{"missing file", filepath.Join(t.TempDir(), "missing.yaml"), "open"},
		{"directory", t.TempDir(), "read"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := Load(test.path)
			if err == nil || errors.Is(err, ErrInvalid) {
				t.Fatalf("Load error = %v, want filesystem error, not ErrInvalid", err)
			}
			var pathError *os.PathError
			if !errors.As(err, &pathError) {
				t.Fatalf("Load error = %v, want wrapped *os.PathError", err)
			}
			assertEqual(t, "PathError.Op", pathError.Op, test.operation)
			assertEqual(t, "PathError.Path", pathError.Path, test.path)
			if test.name == "missing file" && !errors.Is(err, os.ErrNotExist) {
				t.Errorf("Load error = %v, want os.ErrNotExist", err)
			}
			if !strings.Contains(err.Error(), "read configuration") {
				t.Errorf("error lacks read context: %v", err)
			}
		})
	}
}
