package sourcefix

import (
	"context"
	"fmt"
	"strings"
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	codev0 "github.com/codefly-dev/core/generated/go/codefly/services/code/v0"
	"google.golang.org/grpc"
)

type fakeExecutor struct {
	files         map[string]string
	failWriteOn   string
	failed        bool
	reads         map[string]int
	mutateOnRead  map[string]int
	mutateContent map[string]string
	mutateOnFail  map[string]string
}

func (f *fakeExecutor) Execute(_ context.Context, request *codev0.CodeRequest, _ ...grpc.CallOption) (*codev0.CodeResponse, error) {
	switch operation := request.GetOperation().(type) {
	case *codev0.CodeRequest_ReadFile:
		if f.reads == nil {
			f.reads = make(map[string]int)
		}
		path := operation.ReadFile.GetPath()
		f.reads[path]++
		if f.reads[path] == f.mutateOnRead[path] {
			f.files[path] = f.mutateContent[path]
		}
		content, ok := f.files[path]
		return &codev0.CodeResponse{Result: &codev0.CodeResponse_ReadFile{ReadFile: &codev0.ReadFileResponse{Content: content, Exists: ok}}}, nil
	case *codev0.CodeRequest_Fix:
		original, ok := f.files[operation.Fix.GetFile()]
		if !ok {
			return nil, fmt.Errorf("missing")
		}
		fixed := strings.ToUpper(original)
		return &codev0.CodeResponse{Result: &codev0.CodeResponse_Fix{Fix: &codev0.FixResponse{
			Success: true, Content: fixed, Changed: fixed != original, Actions: []string{"fakefmt"},
			BeforeSha256: digest(original), AfterSha256: digest(fixed),
		}}}, nil
	case *codev0.CodeRequest_WriteFile:
		if operation.WriteFile.GetPath() == f.failWriteOn && !f.failed {
			f.failed = true
			if content, ok := f.mutateOnFail[operation.WriteFile.GetPath()]; ok {
				f.files[operation.WriteFile.GetPath()] = content
			}
			return nil, fmt.Errorf("injected write failure")
		}
		f.files[operation.WriteFile.GetPath()] = operation.WriteFile.GetContent()
		return &codev0.CodeResponse{Result: &codev0.CodeResponse_WriteFile{WriteFile: &codev0.WriteFileResponse{Success: true}}}, nil
	default:
		return nil, fmt.Errorf("unexpected operation %T", operation)
	}
}

func TestFixFilesStagesThenCommits(t *testing.T) {
	executor := &fakeExecutor{files: map[string]string{"a.go": "alpha", "b.go": "beta"}}
	report, err := FixFiles(context.Background(), executor, "svc", []string{"a.go", "b.go"}, Options{Mode: basev0.FixMode_FIX_MODE_SAFE})
	if err != nil {
		t.Fatal(err)
	}
	if report.Changed != 2 || report.Written != 2 || executor.files["a.go"] != "ALPHA" || executor.files["b.go"] != "BETA" {
		t.Fatalf("report=%+v files=%v", report, executor.files)
	}
}

func TestFixFilesDryRunDoesNotWrite(t *testing.T) {
	executor := &fakeExecutor{files: map[string]string{"a.go": "alpha"}}
	report, err := FixFiles(context.Background(), executor, "svc", []string{"a.go"}, Options{Mode: basev0.FixMode_FIX_MODE_SAFE, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.Changed != 1 || report.Written != 0 || executor.files["a.go"] != "alpha" {
		t.Fatalf("report=%+v files=%v", report, executor.files)
	}
}

func TestFixFilesRollsBackEarlierWrites(t *testing.T) {
	executor := &fakeExecutor{files: map[string]string{"a.go": "alpha", "b.go": "beta"}, failWriteOn: "b.go"}
	_, err := FixFiles(context.Background(), executor, "svc", []string{"a.go", "b.go"}, Options{Mode: basev0.FixMode_FIX_MODE_SAFE})
	if err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("error = %v", err)
	}
	if executor.files["a.go"] != "alpha" || executor.files["b.go"] != "beta" {
		t.Fatalf("rollback failed: %v", executor.files)
	}
}

func TestFixFilesRejectsEditDuringCommitAndRollsBack(t *testing.T) {
	executor := &fakeExecutor{
		files:         map[string]string{"a.go": "alpha", "b.go": "beta"},
		mutateOnRead:  map[string]int{"b.go": 3},
		mutateContent: map[string]string{"b.go": "user edit"},
	}
	_, err := FixFiles(context.Background(), executor, "svc", []string{"a.go", "b.go"}, Options{Mode: basev0.FixMode_FIX_MODE_SAFE})
	if err == nil || !strings.Contains(err.Error(), "changed during fix commit") || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("error = %v", err)
	}
	if executor.files["a.go"] != "alpha" || executor.files["b.go"] != "user edit" {
		t.Fatalf("concurrent edit or rollback lost: %v", executor.files)
	}
}

func TestFixFilesRollbackPreservesUnknownLiveState(t *testing.T) {
	executor := &fakeExecutor{
		files:        map[string]string{"a.go": "alpha", "b.go": "beta"},
		failWriteOn:  "b.go",
		mutateOnFail: map[string]string{"b.go": "user edit"},
	}
	_, err := FixFiles(context.Background(), executor, "svc", []string{"a.go", "b.go"}, Options{Mode: basev0.FixMode_FIX_MODE_SAFE})
	if err == nil || !strings.Contains(err.Error(), "rollback errors") || !strings.Contains(err.Error(), "refusing to roll back b.go") {
		t.Fatalf("error = %v", err)
	}
	if executor.files["a.go"] != "alpha" || executor.files["b.go"] != "user edit" {
		t.Fatalf("rollback clobbered live state: %v", executor.files)
	}
}

func TestParseModeRequiresExplicitAggressive(t *testing.T) {
	if mode, err := parseMode("safe", false); err != nil || mode != basev0.FixMode_FIX_MODE_SAFE {
		t.Fatalf("safe = %v %v", mode, err)
	}
	if mode, err := parseMode("safe", true); err != nil || mode != basev0.FixMode_FIX_MODE_AGGRESSIVE {
		t.Fatalf("aggressive = %v %v", mode, err)
	}
}
