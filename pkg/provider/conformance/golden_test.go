package conformance

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"testing"

	providerv0 "github.com/codefly-dev/core/generated/go/codefly/services/provider/v0"
	"github.com/codefly-dev/core/provider/manifest"
	"github.com/stretchr/testify/require"
)

var update = flag.Bool("update", false, "rewrite golden artifacts")

// goldenCapture is the sanitized, deterministic projection of a filtered broker
// response. It records what the provider is allowed to see and where secrets
// were routed — never a secret value.
type goldenCapture struct {
	Forwarded  map[string]string  `json:"forwarded"`
	Suppressed []string           `json:"suppressed_presence"`
	Captures   []goldenCaptureRef `json:"captures"`
}

type goldenCaptureRef struct {
	Selector  string `json:"selector"`
	Captured  bool   `json:"captured"`
	Reference string `json:"sink_reference"`
}

// TestGoldenRequestDescriptorDigests pins the canonical digests of every request
// descriptor and the manifest itself, so a change to the wire contract is a
// reviewed diff.
func TestGoldenRequestDescriptorDigests(t *testing.T) {
	h := newBrokerHarness(t)
	digests := map[string]string{}
	for _, descriptor := range h.manifest.Requests {
		digest, err := manifest.RequestDescriptorDigest(descriptor)
		require.NoError(t, err)
		digests[descriptor.ID] = digest
	}
	manifestDigest, err := h.manifest.Digest()
	require.NoError(t, err)
	digests["@manifest"] = manifestDigest

	assertGolden(t, "request_descriptors.json", digests)
}

// TestGoldenCreateProjection pins the filtered create response as seen by the
// provider, proving the safe projection is stable and secret-free.
func TestGoldenCreateProjection(t *testing.T) {
	h := newBrokerHarness(t)
	create := h.createRequest(t)
	session := h.session(t, createAction(t, create), false)
	handle := h.mint(t, create, providerv0.HTTPMethod_HTTP_METHOD_POST)
	response, err := session.Execute(context.Background(), h.execute(handle, create))
	require.NoError(t, err)

	capture := goldenCapture{
		Forwarded:  forwardedBySelector(response),
		Suppressed: response.GetSuppressedPresence(),
	}
	for _, c := range response.GetCaptures() {
		capture.Captures = append(capture.Captures, goldenCaptureRef{
			Selector:  c.GetSelector(),
			Captured:  c.GetCaptured(),
			Reference: c.GetSinkReference().GetReference(),
		})
	}
	sort.Slice(capture.Captures, func(i, j int) bool { return capture.Captures[i].Selector < capture.Captures[j].Selector })

	assertGolden(t, "create_projection.json", capture)
}

// assertGolden compares a value against a committed golden file, rewriting it
// under -update. It hard-fails if a poison value ever reaches a golden artifact.
func assertGolden(t *testing.T, name string, value any) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name)
	encoded, err := json.MarshalIndent(value, "", "  ")
	require.NoError(t, err)
	encoded = append(encoded, '\n')
	assertNoPoison(t, encoded)

	if *update {
		require.NoError(t, os.WriteFile(path, encoded, 0o600))
		return
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err, "missing golden %s; run: go test ./pkg/provider/conformance -update", name)
	require.Equal(t, string(want), string(encoded), "golden %s drifted; run -update to review", name)
}
