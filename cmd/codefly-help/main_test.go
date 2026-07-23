package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/helpprovider"
	"github.com/codefly-dev/llm"
)

// fakeExplainer records the request it receives and returns a canned result, so
// runProvider's protocol handling is tested without a live model or network.
type fakeExplainer struct {
	got    helpprovider.Request
	result string
	err    error
}

func (f *fakeExplainer) explain(_ context.Context, req helpprovider.Request) (string, error) {
	f.got = req
	return f.result, f.err
}

func TestRunProviderPassesRequestToExplainer(t *testing.T) {
	fake := &fakeExplainer{result: "Use this to build demo/api."}
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
	if err := runProvider(t.Context(), bytes.NewReader(request), &output, fake); err != nil {
		t.Fatal(err)
	}
	// The explainer receives the parsed request verbatim (command + context).
	if fake.got.Command != "codefly build service" || !strings.Contains(string(fake.got.Context), `"workspace":"demo"`) {
		t.Errorf("explainer got = %+v", fake.got)
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
	err := runProvider(t.Context(), strings.NewReader(request), &bytes.Buffer{}, &fakeExplainer{})
	if err == nil || !strings.Contains(err.Error(), "protocol version 2") {
		t.Fatalf("error = %v", err)
	}
}

func TestRunProviderRequiresCoreFields(t *testing.T) {
	request := `{"protocol_version":1,"application":"demo","command":"","static_help":"help","context":{}}`
	err := runProvider(t.Context(), strings.NewReader(request), &bytes.Buffer{}, &fakeExplainer{})
	if err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("error = %v", err)
	}
}

// TestLLMExplainerReplaysCassette exercises the real codefly-dev/llm client in
// replay-only mode against a recorded cassette — no API key, no network. It
// skips until a cassette is recorded. To (re)record it, from cli/ load the
// secrets (the gitignored ./llm.secret.env symlink → ~/development/deus/llm.secret.env)
// and run with record mode:
//
//	set -a; . ./llm.secret.env; set +a
//	CLI_HELP_CASSETTE_DIR=$PWD/cmd/codefly-help/testdata/cassette \
//	CLI_HELP_CASSETTE_MODE=record go test ./cmd/codefly-help -run Cassette
func TestLLMExplainerReplaysCassette(t *testing.T) {
	// Recording path: CLI_HELP_CASSETTE_DIR + CLI_HELP_CASSETTE_MODE=record (with a
	// real ANTHROPIC key) records a live call. Default path: replay the committed
	// testdata/cassette offline, skipping until one exists.
	dir := strings.TrimSpace(os.Getenv("CLI_HELP_CASSETTE_DIR"))
	mode := llm.RecordReplayOnly
	if dir != "" && strings.EqualFold(strings.TrimSpace(os.Getenv("CLI_HELP_CASSETTE_MODE")), "record") {
		mode = llm.RecordAlways
	}
	if dir == "" {
		dir = filepath.Join("testdata", "cassette")
	}
	if mode == llm.RecordReplayOnly {
		if entries, err := os.ReadDir(dir); err != nil || len(entries) == 0 {
			t.Skip("no cassette recorded; record with CLI_HELP_CASSETTE_DIR=... CLI_HELP_CASSETTE_MODE=record ANTHROPIC_API_KEY=...")
		}
	}
	client, err := newLLMClient(defaultModel, llm.WithRecorder(dir, mode))
	if err != nil {
		t.Fatal(err)
	}
	ex := &llmExplainer{client: client}
	got, err := ex.explain(t.Context(), helpprovider.Request{
		ProtocolVersion: helpprovider.ProtocolVersion,
		Application:     "codefly",
		Command:         "codefly build service",
		StaticHelp:      "Flags:\n  --push",
		Context:         json.RawMessage(`{"workspace":"demo"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(got) == "" {
		t.Fatal("empty explanation replayed from cassette")
	}
}
