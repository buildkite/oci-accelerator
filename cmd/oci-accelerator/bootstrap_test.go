package main

import (
	"bytes"
	"context"
	"net"
	"strings"
	"testing"
)

func TestBootstrapCLIHelpAndInvalidArguments(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), []string{"bootstrap-ca", "--help"}, &out, &bytes.Buffer{}, net.Listen); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "--secret NAME --configmap NAME") {
		t.Fatal("missing bootstrap usage")
	}
	for _, args := range [][]string{
		{"bootstrap-ca"},
		{"bootstrap-ca", "--secret", "identity"},
		{"bootstrap-ca", "--secret", "secret-sentinel/invalid", "--configmap", "trust"},
		{"bootstrap-ca", "--unknown-secret-sentinel"},
	} {
		out.Reset()
		err := run(context.Background(), args, &out, &bytes.Buffer{}, net.Listen)
		if err == nil || strings.Contains(err.Error(), "secret-sentinel") || out.Len() != 0 {
			t.Fatalf("unsafe invalid-argument result: %v", err)
		}
	}
}
