package orchestration

import (
	"context"
	"encoding/json"
	"github.com/codefly-dev/core/solution/manifest"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCompositionFederationWithholdsOnlyModuleRoutingCredential(t *testing.T) {
	original := map[string]map[string]string{"weekly/app": {
		"CODEFLY__MODULE_REGISTRATION_SECRETS":  "records:private-routing-secret",
		"CODEFLY__SOLUTION_REGISTRATION_SECRET": "own-secret",
		"CODEFLY__API_CONSUMES":                 "[]",
	}}
	flow := &Flow{overrides: original}
	flow.WithCompositionFederation(true)
	got := flow.overridesFor(serviceIn("weekly", "app"))
	if _, exists := got["CODEFLY__MODULE_REGISTRATION_SECRETS"]; exists {
		t.Fatal("routing secret leaked to workload")
	}
	if got["CODEFLY__SOLUTION_REGISTRATION_SECRET"] != "own-secret" {
		t.Fatal("own registration was removed")
	}
	if original["weekly/app"]["CODEFLY__MODULE_REGISTRATION_SECRETS"] == "" {
		t.Fatal("composition lost its private carrier")
	}
	flow.WithCompositionFederation(false)
	if flow.overridesFor(serviceIn("weekly", "app"))["CODEFLY__MODULE_REGISTRATION_SECRETS"] == "" {
		t.Fatal("opt-out changed existing runtime")
	}
}

func TestCompositionFederationUsesPrefixCredentialAndRecoversGatewayRestart(t *testing.T) {
	mints, registrations := 0, 0
	registered := false
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Codefly-Internal-Token") != "internal" {
			t.Error("missing internal transport credential")
		}
		var body map[string]string
		if json.NewDecoder(r.Body).Decode(&body) != nil || body["prefix"] != "records" {
			t.Error("wrong prefix")
		}
		switch r.URL.Path {
		case "/modules/_registration-token":
			mints++
			if r.Header.Get("X-Codefly-Module-Secret") != "secret" {
				t.Error("wrong prefix secret")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "signed-prefix-token", "expiresAt": time.Now().Add(time.Hour)})
		case "/modules/_register":
			registrations++
			if r.Header.Get("X-Codefly-Module-Registration") != "signed-prefix-token" || r.Header.Get("X-Codefly-Module-Secret") != "" {
				t.Error("incorrect registration authority")
			}
			if body["upstream"] != "http://resolved-owner" {
				t.Error("resolved target changed")
			}
			registered = true
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer gateway.Close()
	registration := moduleRegistration{target: manifest.ConsumedAPI{As: "records"}, secret: "secret"}
	for i := 0; i < 2; i++ {
		registered = false // represents the gateway losing its in-memory registry
		if err := registration.register(context.Background(), gateway.Client(), gateway.URL, "http://resolved-owner", "internal"); err != nil {
			t.Fatal(err)
		}
		if !registered {
			t.Fatal("route was not restored")
		}
	}
	if mints != 1 || registrations != 2 {
		t.Fatalf("mints=%d registrations=%d; reuse unexpired token", mints, registrations)
	}
}

func TestCompositionFederationRefusesBadTokenAndDoesNotLogResponseSecrets(t *testing.T) {
	for _, response := range []string{`{"token":"secret","expiresAt":"2000-01-01T00:00:00Z"}`, `{"token":"secret"}`, "invalid json"} {
		calls := 0
		gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; _, _ = w.Write([]byte(response)) }))
		registration := moduleRegistration{target: manifest.ConsumedAPI{As: "records"}, secret: "private"}
		err := registration.register(context.Background(), gateway.Client(), gateway.URL, "http://owner", "internal")
		gateway.Close()
		if err == nil || strings.Contains(err.Error(), "secret") || calls != 1 {
			t.Fatalf("bad mint reached register: calls=%d err=%v", calls, err)
		}
	}
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte("credential-secret"))
	}))
	defer gateway.Close()
	_, err := registrationPost(context.Background(), gateway.Client(), gateway.URL, map[string]string{}, "internal", "X-Codefly-Module-Secret", "secret")
	if err == nil || strings.Contains(err.Error(), "credential-secret") {
		t.Fatal("refusal leaked response body", err)
	}
}

func TestCompositionFederationRemintsRejectedCachedToken(t *testing.T) {
	mints, registrations := 0, 0
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/modules/_registration-token" {
			mints++
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "new-token", "expiresAt": time.Now().Add(time.Hour)})
			return
		}
		registrations++
		if r.Header.Get("X-Codefly-Module-Registration") == "old-token" {
			w.WriteHeader(401)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer gateway.Close()
	r := moduleRegistration{target: manifest.ConsumedAPI{As: "records"}, secret: "secret", token: "old-token", expires: time.Now().Add(time.Hour)}
	if err := r.register(t.Context(), gateway.Client(), gateway.URL, "http://owner", "internal"); err == nil {
		t.Fatal("old token accepted")
	}
	if !r.expires.IsZero() {
		t.Fatal("refused token retained")
	}
	if err := r.register(t.Context(), gateway.Client(), gateway.URL, "http://owner", "internal"); err != nil {
		t.Fatal(err)
	}
	if mints != 1 || registrations != 2 {
		t.Fatal("unexpected refresh count", mints, registrations)
	}
}
func TestCompositionFederationCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r := moduleRegistration{target: manifest.ConsumedAPI{As: "records"}, secret: "secret"}
	if err := r.register(ctx, &http.Client{}, "http://127.0.0.1:1", "http://owner", "internal"); err == nil {
		t.Fatal("cancelled registration succeeded")
	}
}
func TestCompositionFederationReadiness(t *testing.T) {
	flow := &Flow{compositionFederation: true}
	failure := flow.Readiness(t.Context())
	if failure == nil || failure.Predicate != PredicateRequirements {
		t.Fatal("unregistered routes were ready")
	}
}

func TestCompositionFederationDerivation(t *testing.T) {
	stack := newReadinessStack(t)
	flow := stack.flow
	source, err := flow.ServiceFromUnique("app/api")
	if err != nil {
		t.Fatal(err)
	}
	source.ServiceDependencies[0].Name = "auth-gateway"
	source.ServiceDependencies[0].Module = "saas"
	raw, _ := json.Marshal([]manifest.ConsumedAPI{{ID: "records", Module: "data", Service: "store", Endpoint: "grpc", Protocol: "grpc", As: "records"}})
	flow.overrides = map[string]map[string]string{"app/api": {manifest.APIConsumesEnvironmentVariable: string(raw), "CODEFLY__MODULE_REGISTRATION_SECRETS": "records:secret"}}
	got, err := flow.registrations()
	if err != nil || len(got) != 1 || got[0].gatewayModule != "saas" || got[0].target.Service != "store" {
		t.Fatalf("bad derivation: count=%d err=%v", len(got), err)
	}
	flow.overrides["app/api"]["CODEFLY__MODULE_REGISTRATION_SECRETS"] = ""
	if _, err := flow.registrations(); err == nil {
		t.Fatal("missing module credential admitted")
	}
	raw, _ = json.Marshal([]manifest.ConsumedAPI{{ID: "accounts", Module: "saas", Service: "accounts", Endpoint: "rest", Protocol: "rest", As: "accounts"}})
	flow.overrides["app/api"][manifest.APIConsumesEnvironmentVariable] = string(raw)
	got, err = flow.registrations()
	if err != nil || len(got) != 0 {
		t.Fatal("host-owned API required module credential", err)
	}
}
