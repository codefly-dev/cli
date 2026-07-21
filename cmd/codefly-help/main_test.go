package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/helpprovider"
)

func TestRunProviderUsesResponsesAPI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got, want := request.Header.Get("Authorization"), "Bearer secret"; got != want {
			t.Errorf("authorization = %q, want %q", got, want)
		}
		var body struct {
			Model string `json:"model"`
			Input string `json:"input"`
			Store bool   `json:"store"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Model != "test-model" || body.Store {
			t.Errorf("request = %+v", body)
		}
		for _, expected := range []string{"codefly build service", "--push", `"workspace":"demo"`} {
			if !strings.Contains(body.Input, expected) {
				t.Errorf("input does not contain %q: %s", expected, body.Input)
			}
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(writer, `{"output":[{"type":"message","content":[{"type":"output_text","text":"Use this to build demo/api."}]}]}`)
	}))
	defer server.Close()

	request, err := json.Marshal(helpprovider.Request{
		ProtocolVersion: helpprovider.ProtocolVersion,
		Application:     "codefly",
		Command:         "codefly build service",
		StaticHelp:      "Flags:\n  --push",
		Context:         json.RawMessage(`{"workspace":"demo"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err = runProvider(t.Context(), bytes.NewReader(request), &output, server.Client(), providerConfig{
		apiURL: server.URL,
		apiKey: "secret",
		model:  "test-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	var response helpprovider.Response
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.ProtocolVersion != helpprovider.ProtocolVersion || response.Explanation != "Use this to build demo/api." {
		t.Fatalf("response = %+v", response)
	}
}

func TestRunProviderRejectsUnsupportedProtocol(t *testing.T) {
	request := `{"protocol_version":2,"application":"demo","command":"demo run","static_help":"help","context":{}}`
	err := runProvider(t.Context(), strings.NewReader(request), &bytes.Buffer{}, nil, providerConfig{})
	if err == nil || !strings.Contains(err.Error(), "protocol version 2") {
		t.Fatalf("error = %v", err)
	}
}

func TestProviderConfigRequiresAPIKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	if _, err := providerConfigFromEnvironment(); err == nil {
		t.Fatal("provider configuration succeeded without an API key")
	}
}
