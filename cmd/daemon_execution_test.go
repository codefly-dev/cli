package cmd

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
)

func TestGatewayExecutionOptionsRequireExplicitEnablement(t *testing.T) {
	options := gatewayExecutionOptions{
		authorityJWKS: "https://accounts.example.test/keys",
	}
	if _, err := options.childArgs(); err == nil {
		t.Fatal("authority configuration without governed execution was accepted")
	}
	if _, err := options.open(context.Background(), t.TempDir()); err == nil {
		t.Fatal("runtime configuration without governed execution was accepted")
	}
}

func TestGatewayExecutionChildArgsPreserveEveryExporter(t *testing.T) {
	options := gatewayExecutionOptions{
		enabled:         true,
		authorityJWKS:   "https://accounts.example.test/keys",
		authorityIssuer: "https://accounts.example.test",
		stateDir:        filepath.Join(t.TempDir(), "state"),
		exporters:       []string{"example/a:1.0.0", "example/b:2.0.0"},
	}
	got, err := options.childArgs()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"--governed-execution",
		"--execution-authority-jwks", options.authorityJWKS,
		"--execution-authority-issuer", options.authorityIssuer,
		"--execution-state-dir", options.stateDir,
		"--execution-exporter", options.exporters[0],
		"--execution-exporter", options.exporters[1],
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("child args = %#v, want %#v", got, want)
	}
}

func TestGatewayExecutionOptionsRequireCompleteAuthority(t *testing.T) {
	for _, options := range []gatewayExecutionOptions{
		{enabled: true},
		{enabled: true, authorityJWKS: "https://accounts.example.test/keys"},
		{enabled: true, authorityIssuer: "https://accounts.example.test"},
	} {
		if _, err := options.childArgs(); err == nil {
			t.Fatalf("incomplete options accepted: %#v", options)
		}
	}
}
