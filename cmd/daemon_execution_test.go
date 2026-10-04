package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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

// TestGatewayExecutionIsUnavailableWithoutTheIssuersLiveSources: core's
// authenticator verifies against the issuer's live revision and seals, and
// this release has no client for them, so a complete flag set is refused by
// name — before a child process or any durable state — rather than starting
// a gateway that would verify nothing.
func TestGatewayExecutionIsUnavailableWithoutTheIssuersLiveSources(t *testing.T) {
	options := gatewayExecutionOptions{
		enabled:         true,
		authorityIssuer: "https://accounts.example.test",
		stateDir:        filepath.Join(t.TempDir(), "state"),
		exporters:       []string{"example/a:1.0.0", "example/b:2.0.0"},
	}
	_, err := options.childArgs()
	if err == nil || !strings.Contains(err.Error(), "live authorization-revision and seal sources") {
		t.Fatalf("child args error = %v", err)
	}
	_, err = options.open(context.Background(), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "governed execution is unavailable") {
		t.Fatalf("open error = %v", err)
	}
	if _, statErr := os.Stat(options.stateDir); !os.IsNotExist(statErr) {
		t.Fatalf("refused configuration created durable state: %v", statErr)
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
