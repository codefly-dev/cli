package common

import (
	"context"
	"net"
	"testing"

	"github.com/codefly-dev/core/resources"
)

func TestReachable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()

	if !Reachable(addr) {
		t.Errorf("Reachable(%s) = false for an open listener", addr)
	}
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if Reachable(addr) {
		t.Errorf("Reachable(%s) = true after listener closed", addr)
	}
	if Reachable("") {
		t.Error("Reachable(\"\") = true; want false")
	}
}

func TestNormalizeAPIType(t *testing.T) {
	if got, err := NormalizeAPIType("  GRPC "); err != nil || got != "grpc" {
		t.Errorf("NormalizeAPIType(GRPC) = %q, %v; want grpc, nil", got, err)
	}
	if got, err := NormalizeAPIType(""); err != nil || got != "" {
		t.Errorf("NormalizeAPIType(empty) = %q, %v; want empty, nil", got, err)
	}
	if _, err := NormalizeAPIType("bogus"); err == nil {
		t.Error("NormalizeAPIType(bogus) = nil error; want error")
	}
}

func TestFilterEndpoints(t *testing.T) {
	eps := []*resources.Endpoint{
		{Name: "grpc", API: "grpc"},
		{Name: "rest", API: "rest"},
		{Name: "http", API: "http"},
	}
	if got := FilterEndpoints(eps, "rest", ""); len(got) != 1 || got[0].Name != "rest" {
		t.Errorf("filter by type rest = %v", got)
	}
	if got := FilterEndpoints(eps, "", "http"); len(got) != 1 || got[0].Name != "http" {
		t.Errorf("filter by name http = %v", got)
	}
	if got := FilterEndpoints(eps, "", ""); len(got) != 3 {
		t.Errorf("no filter = %d endpoints; want 3", len(got))
	}
	if got := FilterEndpoints(eps, "tcp", ""); len(got) != 0 {
		t.Errorf("filter by absent type = %v; want empty", got)
	}
}

func TestResolveNative(t *testing.T) {
	ctx := context.Background()

	// Standard grpc endpoint resolves to a localhost address + probe target.
	r, err := ResolveNative(ctx, "ws", "mod", "svc", "", &resources.Endpoint{Name: "grpc", API: "grpc"})
	if err != nil {
		t.Fatalf("grpc: unexpected err %v", err)
	}
	if r.External || r.Unsupported || r.Address == "" || r.HostPort == "" {
		t.Errorf("grpc resolved unexpectedly: %+v", r)
	}

	// Name folds to api when the name is itself a supported API and API is empty.
	r, err = ResolveNative(ctx, "ws", "mod", "svc", "", &resources.Endpoint{Name: "rest"})
	if err != nil || r.Unsupported || r.Address == "" {
		t.Errorf("rest-by-name resolved unexpectedly: %+v (err %v)", r, err)
	}

	// Unsupported: empty API and a non-standard name must NOT fabricate an address.
	r, err = ResolveNative(ctx, "ws", "mod", "svc", "", &resources.Endpoint{Name: "metrics"})
	if err != nil {
		t.Fatalf("metrics: unexpected err %v", err)
	}
	if !r.Unsupported || r.Address != "" {
		t.Errorf("non-standard endpoint should be Unsupported with no address, got %+v", r)
	}

	// External endpoints are DNS-resolved at runtime, not port-hashed.
	r, err = ResolveNative(ctx, "ws", "mod", "svc", "", &resources.Endpoint{Name: "grpc", API: "grpc", Visibility: resources.VisibilityExternal})
	if err != nil {
		t.Fatalf("external: unexpected err %v", err)
	}
	if !r.External || r.Address != "" {
		t.Errorf("external endpoint should be External with no address, got %+v", r)
	}
}

func TestResolvedEndpointFromAddress(t *testing.T) {
	for _, test := range []struct {
		address  string
		hostPort string
	}{
		{address: "http://localhost:53231", hostPort: "localhost:53231"},
		{address: "127.0.0.1:6690", hostPort: "127.0.0.1:6690"},
		{address: "http://[::1]:8080", hostPort: "[::1]:8080"},
	} {
		resolved, err := resolvedEndpointFromAddress(test.address)
		if err != nil {
			t.Fatalf("resolvedEndpointFromAddress(%q): %v", test.address, err)
		}
		if resolved.Address != test.address || resolved.HostPort != test.hostPort {
			t.Errorf("resolvedEndpointFromAddress(%q) = %+v", test.address, resolved)
		}
	}
	for _, invalid := range []string{"", "http://localhost", "localhost"} {
		if _, err := resolvedEndpointFromAddress(invalid); err == nil {
			t.Errorf("resolvedEndpointFromAddress(%q) accepted an address without a port", invalid)
		}
	}
}
