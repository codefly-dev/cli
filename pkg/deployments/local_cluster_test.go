package deployments

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

type recordedKubectl struct {
	calls  []string
	stdins []string
	fail   map[string]error
}

func (r *recordedKubectl) run(_ context.Context, stdin string, args ...string) (string, error) {
	call := strings.Join(args, " ")
	r.calls = append(r.calls, call)
	r.stdins = append(r.stdins, stdin)
	for prefix, err := range r.fail {
		if strings.HasPrefix(call, prefix) {
			return "", err
		}
	}
	return "", nil
}

const argoFixture = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: argocd-server
spec:
  template:
    spec:
      containers:
      - image: quay.io/argoproj/argocd:v3.4.1
        name: argocd-server
      initContainers:
        - image: "ghcr.io/dexidp/dex:v2.45.0"
          name: copyutil
`

var fixtureImages = map[string]string{
	"quay.io/argoproj/argocd:v3.4.1": "sha256:" + strings.Repeat("1", 64),
	"ghcr.io/dexidp/dex:v2.45.0":     "sha256:" + strings.Repeat("2", 64),
}

func digestOf(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func fetching(raw string) Fetch {
	return func(context.Context, string) ([]byte, error) { return []byte(raw), nil }
}

// TestEnsureArgoCDUsesAnInstalledArgoAndInstallsOnlyTheReviewedBytes: a
// cluster serving the ApplicationSet API keeps its Argo CD; otherwise the
// installer fetches the manifest at its pinned commit, and applies it — from
// those bytes, every image rewritten to its pinned digest — only when its
// SHA-256 is the reviewed one. A manifest that moved applies nothing at all.
func TestEnsureArgoCDUsesAnInstalledArgoAndInstallsOnlyTheReviewedBytes(t *testing.T) {
	present := &recordedKubectl{}
	installed, err := EnsureArgoCD(context.Background(), present.run, fetching("unused"))
	if err != nil || installed || len(present.calls) != 1 {
		t.Fatalf("an installed Argo CD was reinstalled: installed=%v err=%v calls=%v", installed, err, present.calls)
	}

	// The attack: the tag moved, or the file changed upstream.
	moved := &recordedKubectl{fail: map[string]error{"get crd": errors.New("NotFound")}}
	if _, err = EnsureArgoCD(context.Background(), moved.run, fetching(argoFixture)); err == nil || !strings.Contains(err.Error(), "not the reviewed") {
		t.Fatalf("a manifest with another digest was not refused: %v", err)
	}
	if len(moved.calls) != 1 {
		t.Fatalf("a refused manifest still reached the cluster: %v", moved.calls)
	}

	kubectl := &recordedKubectl{}
	var fetched string
	fetch := func(_ context.Context, url string) ([]byte, error) { fetched = url; return []byte(argoFixture), nil }
	if err = installArgoCD(context.Background(), kubectl.run, fetch, digestOf(argoFixture), fixtureImages); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fetched, "/"+argoCDCommit+"/manifests/install.yaml") {
		t.Fatalf("the manifest was fetched from %s, not its pinned commit", fetched)
	}
	joined := strings.Join(kubectl.calls, "\n")
	for _, want := range []string{"apply --server-side --force-conflicts -n argocd -f -", "wait --for=condition=Ready pod --all -n argocd"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("the install did not run %q:\n%s", want, joined)
		}
	}
	applied := kubectl.stdins[1]
	for _, want := range []string{"image: quay.io/argoproj/argocd:v3.4.1@sha256:" + strings.Repeat("1", 64), "image: ghcr.io/dexidp/dex:v2.45.0@sha256:" + strings.Repeat("2", 64)} {
		if !strings.Contains(applied, want) {
			t.Fatalf("the applied manifest does not pin %q:\n%s", want, applied)
		}
	}
}

// TestAnArgoManifestRunningAnUnpinnedImageIsRefused: an image the table does
// not pin — one the reviewed manifest never ran — refuses the install.
func TestAnArgoManifestRunningAnUnpinnedImageIsRefused(t *testing.T) {
	extra := argoFixture + "---\nkind: Deployment\nspec:\n  template:\n    spec:\n      containers:\n        - image: docker.io/evil/miner:latest\n"
	kubectl := &recordedKubectl{}
	if err := installArgoCD(context.Background(), kubectl.run, fetching(extra), digestOf(extra), fixtureImages); err == nil || !strings.Contains(err.Error(), "docker.io/evil/miner:latest") {
		t.Fatalf("an unpinned image was not refused by name: %v", err)
	}
	if len(kubectl.calls) != 0 {
		t.Fatalf("a refused manifest reached the cluster: %v", kubectl.calls)
	}
}
