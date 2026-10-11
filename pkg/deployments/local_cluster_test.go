package deployments

import (
	"context"
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

// TestEnsureArgoCDUsesAnInstalledArgoAndInstallsTheSharedReleaseOtherwise: a
// cluster that serves the ApplicationSet API keeps its Argo CD; one that does
// not gets the release the k3d qualification installs, through the one
// installer both use, and the run waits for it.
func TestEnsureArgoCDUsesAnInstalledArgoAndInstallsTheSharedReleaseOtherwise(t *testing.T) {
	present := &recordedKubectl{}
	installed, err := EnsureArgoCD(context.Background(), present.run)
	if err != nil || installed || len(present.calls) != 1 {
		t.Fatalf("an installed Argo CD was reinstalled: installed=%v err=%v calls=%v", installed, err, present.calls)
	}
	absent := &recordedKubectl{fail: map[string]error{"get crd": errors.New("NotFound")}}
	installed, err = EnsureArgoCD(context.Background(), absent.run)
	if err != nil || !installed {
		t.Fatalf("installed=%v err=%v", installed, err)
	}
	joined := strings.Join(absent.calls, "\n")
	for _, want := range []string{"apply -f -", "apply --server-side --force-conflicts -n argocd -f " + ArgoCDInstallManifest, "wait --for=condition=Ready pod --all -n argocd"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("the install did not run %q:\n%s", want, joined)
		}
	}
	if !strings.Contains(absent.stdins[1], "name: argocd") {
		t.Fatalf("the argocd namespace was not applied: %q", absent.stdins[1])
	}
}

// TestDeliveryAccountIsTheAuthorityNamespacesDeliveryAccount: the namespace
// the authority Job runs in and the account it runs as, which a cell's
// platform provides and the rendered tree cannot, with no default token.
func TestDeliveryAccountIsTheAuthorityNamespacesDeliveryAccount(t *testing.T) {
	kubectl := &recordedKubectl{}
	if err := EnsureDeliveryAccount(context.Background(), kubectl.run); err != nil {
		t.Fatal(err)
	}
	manifest := kubectl.stdins[0]
	for _, want := range []string{"kind: Namespace\nmetadata:\n  name: platform-authority", "kind: ServiceAccount\nmetadata:\n  name: delivery\n  namespace: platform-authority", "automountServiceAccountToken: false"} {
		if !strings.Contains(manifest, want) {
			t.Fatalf("the delivery account manifest lacks %q:\n%s", want, manifest)
		}
	}
}
