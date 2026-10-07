package cmd

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
)

func TestGatewayExecutionOptionsRequireExplicitEnablement(t *testing.T) {
	// Authority configured, governed execution not enabled. The ISSUER carries
	// that now: the JWKS URL this used to set is gone with the verifier.
	options := gatewayExecutionOptions{authorityIssuer: "https://accounts.example.test"}
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
	// A key source is no longer part of a complete authority: this process
	// verifies nothing. The issuer still is — it is what the recorder compares
	// a verified capability's own issuer against.
	if _, err := (gatewayExecutionOptions{enabled: true}).childArgs(); err == nil {
		t.Fatal("options with no issuer were accepted")
	}
	complete := gatewayExecutionOptions{enabled: true, authorityIssuer: "https://accounts.example.test"}
	if _, err := complete.childArgs(); err != nil {
		t.Fatalf("complete options rejected: %v", err)
	}
}
