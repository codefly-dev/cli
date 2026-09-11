package ci

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestReplayPreservesSelectionAndRejectsAlterations(t *testing.T) {
	for _, scenario := range []string{"roundtrip", "reasons", "paths", "tasks", "topology", "fingerprint", "revision", "content", "ignored", "all", "bounds", "schema"} {
		t.Run(scenario, func(t *testing.T) {
			root, workspace := loadSchedulerFixture(t)
			runCacheTestGit(t, root, "init")
			runCacheTestGit(t, root, "add", ".")
			runCacheTestGit(t, root, "-c", "user.name=CI Test", "-c", "user.email=ci@example.com", "commit", "-m", "fixture")
			options := PlanOptions{ChangedFiles: []string{"modules/management/services/organization/code/deleted.go"}}
			ctx := context.Background()
			plan, err := BuildPlan(ctx, workspace, options)
			if err != nil {
				t.Fatal(err)
			}
			saved, err := buildReplayPlan(ctx, workspace, plan)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "reasons":
				saved.Selection.Services[0].Reasons = []string{"altered"}
			case "paths":
				saved.Selection.ChangedFiles = []string{"elsewhere"}
			case "tasks":
				saved.Selection.Services = saved.Selection.Services[:1]
			case "topology":
				saved.Execution[len(saved.Execution)-1].Plan.Edges = nil
			case "fingerprint":
				saved.Execution[0].Fingerprint = "altered"
			case "revision":
				saved.Candidate = "stale"
			case "content":
				writeCacheTestFile(t, filepath.Join(root, "untracked.go"), "package changed")
			case "ignored":
				writeCacheTestFile(t, filepath.Join(root, ".git", "info", "exclude"), "ignored.input\n")
				writeCacheTestFile(t, filepath.Join(root, "ignored.input"), "changed")
			case "all":
				options.All = true
			case "bounds":
				options = PlanOptions{}
			case "schema":
				saved.Schema = "future"
			}
			path := filepath.Join(t.TempDir(), "plan.json")
			payload, err := json.Marshal(saved)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, payload, 0o600); err != nil {
				t.Fatal(err)
			}
			actual, err := readReplayPlan(ctx, workspace, path, &options)
			if scenario == "roundtrip" {
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(actual, plan) {
					t.Fatalf("selection changed: %#v", actual)
				}
			} else if err == nil {
				t.Fatal("accepted altered or incompatible plan")
			}
		})
	}
}

func TestReplayContentFollowsSymlinksAndRejectsCycles(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "source")
	writeCacheTestFile(t, target, "before")
	if err := os.Symlink(target, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	before, err := replayContent(context.Background(), root, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	writeCacheTestFile(t, target, "after")
	after, err := replayContent(context.Background(), root, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("symlink target contents were not validated")
	}
	if err := os.Symlink(root, filepath.Join(root, "cycle")); err != nil {
		t.Fatal(err)
	}
	if _, err := replayContent(context.Background(), root, map[string]bool{}); err == nil || !strings.Contains(err.Error(), "cyclic") {
		t.Fatalf("cycle: %v", err)
	}
}

func TestReplayContentRejectsCancellationAndSockets(t *testing.T) {
	root, err := os.MkdirTemp("", "ci-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := replayContent(ctx, root, map[string]bool{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	listener, err := net.Listen("unix", filepath.Join(root, "socket"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if _, err := replayContent(context.Background(), root, map[string]bool{}); err == nil || !strings.Contains(err.Error(), "unsupported source file type") {
		t.Fatalf("socket: %v", err)
	}
}
