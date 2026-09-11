package ci

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManifestOwnershipGateUsesGitBaseline(t *testing.T) {
	for _, scenario := range []string{"working tree drop", "committed drop", "retired file", "missing CI baseline", "invalid baseline", "linked tools drop"} {
		t.Run(scenario, func(t *testing.T) {
			root, workspace := loadComposedPlanFixture(t)
			manifest := "module/tools/base-manifest.json"
			if scenario == "linked tools drop" {
				if err := os.Mkdir(filepath.Join(root, "module", "metadata"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("metadata", filepath.Join(root, "module", "tools")); err != nil {
					t.Fatal(err)
				}
				manifest = "module/metadata/base-manifest.json"
			}
			source := "services/frontend/code/src/example.ts"
			digest := sha256.Sum256([]byte("export const example = 1;\n"))
			writeCacheTestFile(t, filepath.Join(root, manifest), `{"files":{"`+source+`":"`+hex.EncodeToString(digest[:])+`"}}`)
			runCacheTestGit(t, root, "init")
			runCacheTestGit(t, root, "add", ".")
			runCacheTestGit(t, root, "-c", "user.name=CI Test", "-c", "user.email=ci@example.com", "commit", "-m", "owned baseline")
			writeCacheTestFile(t, filepath.Join(root, manifest), `{"files":{}}`)
			options := PlanOptions{RepoRoot: root, ChangedFiles: []string{manifest}}
			expected := "dropped ownership"
			switch scenario {
			case "committed drop":
				runCacheTestGit(t, root, "add", ".")
				runCacheTestGit(t, root, "-c", "user.name=CI Test", "-c", "user.email=ci@example.com", "commit", "-m", "drop ownership")
				options.Base = "HEAD~1"
				t.Setenv("CI", "true")
			case "retired file":
				if err := os.Remove(filepath.Join(root, "module", source)); err != nil {
					t.Fatal(err)
				}
				options.ChangedFiles = append(options.ChangedFiles, "module/"+source)
				expected = ""
			case "missing CI baseline":
				t.Setenv("CI", "true")
				expected = "--base is required"
			case "invalid baseline":
				options.Base = "missing-revision"
				expected = "resolve manifest ownership baseline"
			}
			plan, err := BuildPlan(context.Background(), workspace, options)
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.IntegrityInputs) != 1 {
				t.Fatalf("inputs = %+v", plan.IntegrityInputs)
			}
			reporter := fixedCIReporter(t, plan)
			err = executeCIPhase(context.Background(), reporter, workspace, plan, "verify", nil, true)
			if expected == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), expected) {
				t.Fatalf("error=%v want %q", err, expected)
			}
			report := reporter.Finalize(err)
			if len(report.Tasks) != 1 || report.Tasks[0].Integrity == nil {
				t.Fatalf("tasks=%+v", report.Tasks)
			}
			evidence := report.Tasks[0].Integrity
			wantFailed := 0
			if expected != "" {
				wantFailed = 1
			}
			if evidence.GuardedModules != 1 || evidence.FailedModules != wantFailed {
				t.Fatalf("evidence=%+v", evidence)
			}
		})
	}
}
