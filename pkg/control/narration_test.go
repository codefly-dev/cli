package control

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/codefly-dev/core/wool"
)

// recordingNarration collects what a plane's flows log.
type recordingNarration struct {
	mu    sync.Mutex
	lines []string
}

func (r *recordingNarration) Process(log *wool.Log) {
	if log == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, log.String())
}

func (r *recordingNarration) joined() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.lines, "\n")
}

// Discarding keeps narration off stdout, but it also swallows the playbook's
// failure warnings and everything wrapped out of Flow.Start — which is what an
// operator reads when a run driven over MCP fails. An embedder that owns stdout
// for a protocol has a safe destination for those lines and must be able to
// name it instead of losing them.
func TestWithNarrationRoutesFlowLogsToTheEmbedder(t *testing.T) {
	recorder := &recordingNarration{}
	routed := &planeImpl{}
	WithNarration(recorder)(routed)

	wool.Get(routed.narrationContext(context.Background())).In("plane").Error("service wiki/backend failed")

	if !strings.Contains(recorder.joined(), "service wiki/backend failed") {
		t.Errorf("narration did not reach the processor the embedder named; it had %q", recorder.joined())
	}
}

// Unset, the plane still discards rather than falling back to wool's Console,
// which prints to the stdout an MCP server serves JSON-RPC on.
func TestNarrationWithoutAProcessorStaysOffStdout(t *testing.T) {
	capture := captureStdout(t)

	wool.Get((&planeImpl{}).narrationContext(context.Background())).In("plane").Error("must not reach stdout")

	if written := capture.written(); written != "" {
		t.Errorf("a plane with no narration processor wrote %q to stdout", written)
	}
}
