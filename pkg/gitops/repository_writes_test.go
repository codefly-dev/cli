package gitops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepositoryBindingGuardsEveryLocalWrite(t *testing.T) {
	for _, write := range []string{"snapshot", "promotion branch", "review ref", "base", "advertise after inspection"} {
		t.Run(write, func(t *testing.T) {
			ctx, workspace, declared := developerPublishWorkspace(t)
			installDeveloperSigning(t, &developerSigning{signer: &fakeSigner{nonce: true}})
			request := PublishRequest{Module: "payments", Environment: "production", Local: true}
			prepared, err := preparePublish(ctx, workspace, &request, "", false)
			if err != nil {
				t.Fatal(err)
			}
			defer prepared.cleanup()
			gitRun(t, prepared.dir, "-c", "user.name=t", "-c", "user.email=t@example.test", "commit", "--quiet", "--allow-empty", "-m", "promotion")
			commit := gitOutput(t, prepared.dir, "rev-parse", "HEAD")
			before := gitOutput(t, "", "--git-dir", declared, "for-each-ref")
			replacement := filepath.Join(t.TempDir(), "replacement.git")
			gitRun(t, "", "clone", "--quiet", "--mirror", declared, replacement)
			if err = os.Rename(declared, declared+".original"); err != nil {
				t.Fatal(err)
			}
			if err = os.Rename(replacement, declared); err != nil {
				t.Fatal(err)
			}
			bound := prepared.plan.LocalRepository
			switch write {
			case "snapshot":
				err = publishServiceSnapshot(ctx, prepared.dir, "payments", "production", commit, &deliveryPublication{localRepository: bound, repositoryURL: prepared.plan.Repository})
			case "promotion branch":
				err = bound.publishRef(ctx, prepared.plan.Repository, prepared.dir, commit, "refs/heads/"+prepared.plan.PromotionBranch)
			case "review ref":
				_, _, err = openOrUpdatePullRequest(ctx, prepared, &request, commit)
			case "base":
				err = promoteLocally(ctx, prepared, commit)
			case "advertise after inspection":
				var advertised *preparedRepository
				advertised, err = preparePublish(ctx, workspace, &request, "", true, bound)
				if advertised != nil {
					advertised.cleanup()
				}
			}
			if err == nil || !strings.Contains(err.Error(), "changed") {
				t.Fatalf("write to a replacement was not refused: %v", err)
			}
			for _, repository := range []string{declared, declared + ".original"} {
				if got := gitOutput(t, "", "--git-dir", repository, "for-each-ref"); got != before {
					t.Fatalf("a repository's refs changed: %s", got)
				}
			}
		})
	}
}
