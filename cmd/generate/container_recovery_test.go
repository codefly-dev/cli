package generate

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/runners/dockerrun"
)

// newRecoveryWorkspace writes a workspace declaring `local` with the given
// naming scope, makes it the working directory, and isolates CODEFLY_HOME so
// the resolved ownership never depends on the developer's own home.
func newRecoveryWorkspace(t *testing.T, namingScope string) string {
	t.Helper()
	root := t.TempDir()
	declaration := `name: recovery
layout: modules
environments:
    - name: local
`
	if namingScope != "" {
		declaration += "      naming-scope: " + namingScope + "\n"
	}
	writeTestFile(t, filepath.Join(root, "workspace.codefly.yaml"), declaration)
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	t.Setenv(dockerrun.ContainerRecoveryScopeEnvironment, "")
	t.Chdir(root)
	resetContainerRecoveryOnce(t)
	return root
}

// resetContainerRecoveryOnce lets each test project again. Projection is
// deliberately once-per-process so a per-endpoint buildDescriptorSet loop does
// not re-resolve, which means tests sharing the binary would otherwise inherit
// whichever workspace projected first.
func resetContainerRecoveryOnce(t *testing.T) {
	t.Helper()
	containerRecoveryOnce = sync.Once{}
}

// markerFields splits the projected marker into the exact scope hash and the
// durable namespace. The two sweeps key on different halves, so a test that
// cares which one can match has to look at them separately.
func markerFields(t *testing.T) (scopeID, namespace string) {
	t.Helper()
	identity := dockerrun.InheritedContainerRecoveryScope()
	if identity == "" {
		return "", ""
	}
	scopeID, namespace, ok := strings.Cut(identity, ":")
	if !ok {
		t.Fatalf("projected identity %q is not <scope>:<namespace>", identity)
	}
	return scopeID, namespace
}

// TestGenerateProjectsTheIdentityARunResolves is the whole point of projecting
// here. `generate` builds its containers in the CLI process, so nothing else
// ever labels them, and a run's exact-scope sweep compares a hash of home,
// workspace and naming scope. The identity must therefore be byte-identical to
// the one `codefly run` resolves for the same workspace.
func TestGenerateProjectsTheIdentityARunResolves(t *testing.T) {
	root := newRecoveryWorkspace(t, "from-yaml")

	got, err := containerRecoveryScope(context.Background())
	if err != nil {
		t.Fatalf("containerRecoveryScope: %v", err)
	}
	want, err := dockerrun.NewContainerRecoveryScope(os.Getenv(resources.CodeflyHomeEnv), root, "from-yaml")
	if err != nil {
		t.Fatalf("NewContainerRecoveryScope: %v", err)
	}
	if got != want {
		t.Fatalf("resolved scope = %+v, want the scope a run resolves %+v", got, want)
	}

	// The declared naming scope must actually reach the hash. Without it the
	// label would be well-formed, stable, and matched by no sweep the workspace
	// ever runs.
	unscoped, err := dockerrun.NewContainerRecoveryScope(os.Getenv(resources.CodeflyHomeEnv), root, "")
	if err != nil {
		t.Fatalf("NewContainerRecoveryScope: %v", err)
	}
	if got == unscoped {
		t.Fatal("resolved scope ignores the workspace's declared naming scope")
	}
}

// TestGenerateLeftoversSurviveARunThatRenamesTheScope is the regression test for
// the gap that exact-scope matching alone leaves open. A run is free to pick a
// different naming scope (--naming-scope, a non-local --env, or the invocation
// id --temporary-ports generates), and ReapStaleContainers compares a hash that
// includes it — so a leftover labeled under the declared scope is invisible to
// that sweep. What rescues it is the durable namespace, which covers home and
// workspace only; `generate` marks its containers ephemeral so
// ReapDisposableContainers, keyed on that namespace, collects them. This test
// holds the property that makes it work: renaming the scope must change the
// exact hash and leave the namespace alone.
func TestGenerateLeftoversSurviveARunThatRenamesTheScope(t *testing.T) {
	root := newRecoveryWorkspace(t, "from-yaml")
	home := os.Getenv(resources.CodeflyHomeEnv)

	generated, err := dockerrun.NewContainerRecoveryScope(home, root, "from-yaml")
	if err != nil {
		t.Fatalf("NewContainerRecoveryScope: %v", err)
	}
	if err = dockerrun.SetContainerRecoveryScope(generated); err != nil {
		t.Fatalf("SetContainerRecoveryScope: %v", err)
	}
	generatedScope, generatedNamespace := markerFields(t)

	// What a later `codefly run --naming-scope=pr-123` in the same workspace
	// resolves.
	renamed, err := dockerrun.NewContainerRecoveryScope(home, root, "pr-123")
	if err != nil {
		t.Fatalf("NewContainerRecoveryScope: %v", err)
	}
	if err = dockerrun.SetContainerRecoveryScope(renamed); err != nil {
		t.Fatalf("SetContainerRecoveryScope: %v", err)
	}
	renamedScope, renamedNamespace := markerFields(t)

	if generatedNamespace == "" || renamedNamespace == "" {
		t.Skip("host has no durable identity; cross-scope recovery is unavailable here by design")
	}
	if generatedScope == renamedScope {
		t.Fatal("renaming the naming scope left the exact scope hash unchanged; the sweeps would be indistinguishable")
	}
	if generatedNamespace != renamedNamespace {
		t.Fatalf("durable namespace changed with the naming scope (%q vs %q); a run that renames the scope could never collect a generate leftover",
			generatedNamespace, renamedNamespace)
	}
}

// TestGenerateProjectsTheMarkerIntoTheProcess covers the projection itself:
// core stamps the labels from the process marker, read back through
// InheritedContainerRecoveryScope, so an unprojected marker means unlabeled
// containers no matter what was resolved.
func TestGenerateProjectsTheMarkerIntoTheProcess(t *testing.T) {
	root := newRecoveryWorkspace(t, "from-yaml")

	projectContainerRecovery(context.Background())
	projected := dockerrun.InheritedContainerRecoveryScope()
	if projected == "" {
		t.Fatal("generate projected no container recovery marker")
	}

	want, err := dockerrun.NewContainerRecoveryScope(os.Getenv(resources.CodeflyHomeEnv), root, "from-yaml")
	if err != nil {
		t.Fatalf("NewContainerRecoveryScope: %v", err)
	}
	if err = dockerrun.SetContainerRecoveryScope(want); err != nil {
		t.Fatalf("SetContainerRecoveryScope: %v", err)
	}
	if identity := dockerrun.InheritedContainerRecoveryScope(); projected != identity {
		t.Fatalf("projected identity = %q, want %q", projected, identity)
	}

	// Negative control. Without it the assertion above holds for any marker
	// whatsoever, since both sides are stamped from the same inputs — it would
	// pass even if the marker ignored the resolved scope entirely.
	other, err := dockerrun.NewContainerRecoveryScope(os.Getenv(resources.CodeflyHomeEnv), root, "some-other-scope")
	if err != nil {
		t.Fatalf("NewContainerRecoveryScope: %v", err)
	}
	if err = dockerrun.SetContainerRecoveryScope(other); err != nil {
		t.Fatalf("SetContainerRecoveryScope: %v", err)
	}
	if identity := dockerrun.InheritedContainerRecoveryScope(); identity == projected {
		t.Fatal("a different scope produced the same marker; the marker does not encode the resolved scope")
	}
}

// TestGenerateResolvesOwnershipOncePerCommand pins the projection to one
// resolution. `generate contracts` and `generate client` call buildDescriptorSet
// once per grpc endpoint, so a per-call resolution re-read the workspace and
// re-warned on every endpoint. Resolving once is also what keeps the identity
// stable for the whole command.
func TestGenerateResolvesOwnershipOncePerCommand(t *testing.T) {
	newRecoveryWorkspace(t, "from-yaml")

	projectContainerRecovery(context.Background())
	first := dockerrun.InheritedContainerRecoveryScope()
	if first == "" {
		t.Fatal("generate projected no container recovery marker")
	}

	// A second endpoint in the same command, resolved from somewhere else
	// entirely. The command's ownership must not move underneath the containers
	// it already labeled.
	t.Chdir(t.TempDir())
	projectContainerRecovery(context.Background())
	if identity := dockerrun.InheritedContainerRecoveryScope(); identity != first {
		t.Fatalf("ownership re-resolved mid-command: %q then %q", first, identity)
	}
}

// calledIn returns the names of every function called somewhere inside body,
// whether through a receiver or not.
func calledIn(body ast.Node) map[string]bool {
	called := map[string]bool{}
	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fun := call.Fun.(type) {
		case *ast.SelectorExpr:
			called[fun.Sel.Name] = true
		case *ast.Ident:
			called[fun.Name] = true
		}
		return true
	})
	return called
}

// TestEveryGenerateContainerIsOwnedAndDisposable holds the two calls that make
// a container this command builds recoverable to the function that builds it.
//
// Neither can be left out without going silent. Without the projection the
// container carries no recovery label at all and no sweep on any Core
// generation matches it; without WithEphemeral it carries one, but only the
// exact-scope sweep can see it — so a later run that chose a different naming
// scope walks past it forever. Nothing fails in either case: a label that
// matches nothing looks exactly like a label that matches.
//
// The Docker qualification drives one such container end to end. This is what
// makes that proof transfer to the command rather than to the one call site it
// happened to exercise.
func TestEveryGenerateContainerIsOwnedAndDisposable(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read cmd/generate: %v", err)
	}
	fset := token.NewFileSet()
	builders := 0
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(fset, name, nil, 0)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", name, parseErr)
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			called := calledIn(function.Body)
			if !called["NewDockerEnvironment"] {
				continue
			}
			builders++
			if !called["projectContainerRecovery"] {
				t.Errorf("%s: %s builds a container without projecting container recovery ownership; it would carry no recovery label and no sweep could ever collect it",
					name, function.Name.Name)
			}
			if !called["WithEphemeral"] {
				t.Errorf("%s: %s builds a container that is not ephemeral; only a run keeping generate's exact naming scope could ever collect a leftover",
					name, function.Name.Name)
			}
		}
	}
	// The assertions above are vacuously true for a package that builds no
	// containers, which is also what a renamed constructor looks like.
	if builders == 0 {
		t.Fatal("no function in cmd/generate builds a docker environment; this test no longer holds anything")
	}
}

// TestGenerateOutsideAWorkspaceProjectsNothing holds the degradation. Ownership
// is a hash of a workspace that is not there, and substituting one would stamp
// containers with a durable identity no sweep can match — strictly worse than
// no label. Generating outside a workspace must still work.
func TestGenerateOutsideAWorkspaceProjectsNothing(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	t.Setenv(dockerrun.ContainerRecoveryScopeEnvironment, "")
	t.Chdir(t.TempDir())
	resetContainerRecoveryOnce(t)

	if _, err := containerRecoveryScope(context.Background()); err == nil {
		t.Fatal("containerRecoveryScope resolved an ownership outside any workspace")
	}
	projectContainerRecovery(context.Background())
	if identity := dockerrun.InheritedContainerRecoveryScope(); identity != "" {
		t.Fatalf("projected %q outside a workspace, want no marker", identity)
	}
}
