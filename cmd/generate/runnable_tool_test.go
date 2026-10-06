package generate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	runnablespkg "github.com/codefly-dev/cli/pkg/runnables"
	runnablev0 "github.com/codefly-dev/core/generated/go/codefly/runnable/v0"
	corerunnable "github.com/codefly-dev/core/runnable"
	"google.golang.org/protobuf/proto"
)

func TestToolExposureSurvivesGenerationAndPreparedDelivery(t *testing.T) {
	declared := conformingOperation()
	declared.Tool = &runnablev0.ToolExposure{Name: "apply_text", Description: "Apply text supplied by the caller.", Effect: runnablev0.ToolExposure_EFFECT_MUTATION}
	root, moduleDir := saveRunnableFixture(t, context.Background(), descriptorSet(t, ingestionFile(declared)), "0.1.0", connectEndpoint())
	t.Chdir(root)
	resetRunnablesFlags(t)
	resetRunnableBindingsFlags(t)
	if err := RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}); err != nil {
		t.Fatal(err)
	}
	entry := readIndex(t, moduleDir).Operations[0]
	operation := readDerivedOperation(t, moduleDir, entry)
	if operation.Tool == nil || operation.Tool.Name != declared.Tool.Name || operation.Tool.Effect != "EFFECT_MUTATION" {
		t.Fatalf("derived exposure lost: %+v", operation.Tool)
	}
	if err := RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "configurations", "local", RunnableBindingsGroup+".env"))
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
			continue
		}
		_, value, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("invalid binding line: %s", line)
		}
		binding, err := corerunnable.DecodePrepared([]byte(value))
		if err != nil {
			t.Fatal(err)
		}
		tool, err := corerunnable.ToolFromPrepared(binding)
		if err != nil {
			t.Fatal(err)
		}
		if !proto.Equal(tool, declared.Tool) {
			t.Fatalf("prepared exposure differs: %v", tool)
		}
		count++
	}
	if count != 1 {
		t.Fatalf("expected one prepared operation, got %d", count)
	}
	runnableBindingsCheck = true
	if err := RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil); err != nil {
		t.Fatalf("unchanged generation drifted: %v", err)
	}
}

func TestPreparedToolCannotInventAnEffect(t *testing.T) {
	document := newOperationDocument(&corerunnable.OperationSpec{})
	document.AttemptTimeout, document.TotalTimeout, document.Backoff = "1s", "10s", "1s"
	document.Completion = "COMPLETION_CALL"
	for _, effect := range []string{"", "EFFECT_UNSPECIFIED", "READ", "99"} {
		document.Tool = &runnablespkg.ToolExposure{Name: "read_item", Description: "Read an item.", Effect: effect}
		if _, err := preparedPolicy(document); err == nil {
			t.Fatalf("accepted effect %q", effect)
		}
	}
	document.Tool = nil
	policy, err := preparedPolicy(document)
	if err != nil {
		t.Fatal(err)
	}
	if policy.Tool != nil {
		t.Fatal("unexposed operation became a tool")
	}
}
