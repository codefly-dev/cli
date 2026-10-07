package effect

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	gatewayv1 "github.com/codefly-dev/core/generated/go/mind/gateway/v1"
)

// contractMethods is every entry point the generated Gateway contract has,
// read from the descriptor rather than written down. That is the whole point:
// a method added to the proto appears here without anyone remembering to add
// it, so the assertions below start failing the moment the contract grows.
func contractMethods() []string {
	descriptor := gatewayv1.Gateway_ServiceDesc
	names := make([]string, 0, len(descriptor.Methods)+len(descriptor.Streams))
	for _, method := range descriptor.Methods {
		names = append(names, method.MethodName)
	}
	for _, stream := range descriptor.Streams {
		names = append(names, stream.StreamName)
	}
	sort.Strings(names)
	return names
}

// Every method of the contract declares what it does, and the inventory names
// nothing the contract does not have.
//
// The first half is the gate against an ungated RPC: a new method with no
// class is refused by Admit at runtime, and fails here at test time, which is
// where it should be caught. The second half is the gate against rot — a
// renamed or deleted RPC leaving an entry behind that silently classifies
// nothing.
func TestTheInventoryIsExactlyTheContract(t *testing.T) {
	contract := contractMethods()
	for _, method := range contract {
		if _, known := ClassOf(method); !known {
			t.Errorf("Gateway method %q declares no effect class: add it to pkg/gateway/effect, as an Observation only if it reads", method)
		}
	}
	inContract := make(map[string]bool, len(contract))
	for _, method := range contract {
		inContract[method] = true
	}
	for _, named := range Methods() {
		if !inContract[named] {
			t.Errorf("the inventory names %q, which the Gateway contract does not have", named)
		}
	}
	if len(contract) != len(Methods()) {
		t.Errorf("contract has %d entry points, inventory names %d", len(contract), len(Methods()))
	}
}

// An unknown method is REFUSED, not permitted. This is the fail-closed half of
// the design: the inventory test above cannot run in production, so the rule
// itself must not serve a method it cannot classify.
func TestAnUnclassifiedMethodIsRefused(t *testing.T) {
	err := Admit(governedContext(t), "/mind.gateway.v1.Gateway/MethodAddedTomorrow")
	if err == nil {
		t.Fatal("an unclassified method was admitted")
	}
	// And refused even when the request is not governed at all: the gateway
	// does not know whether it performs an effect.
	if err := Admit(ungovernedContext(), "MethodAddedTomorrow"); err == nil {
		t.Fatal("an unclassified method was admitted for an ungoverned request")
	}
}

// Every classified method agrees with the rule: an effect is refused when
// governed, an observation is not, and neither is refused when the request
// carries no capability.
func TestTheRuleHoldsForEveryMethodOfTheContract(t *testing.T) {
	governed := governedContext(t)
	ungoverned := ungovernedContext()
	effects, observations := 0, 0
	for _, method := range contractMethods() {
		class, known := ClassOf(method)
		if !known {
			continue // reported by TestTheInventoryIsExactlyTheContract
		}
		if err := Admit(ungoverned, method); err != nil {
			t.Errorf("%s: an ungoverned request was refused: %v", method, err)
		}
		err := Admit(governed, method)
		switch class {
		case Effect:
			effects++
			if err == nil {
				t.Errorf("%s: a governed EFFECT was admitted, so it would run unrecorded", method)
			}
		case Observation:
			observations++
			if err != nil {
				t.Errorf("%s: a governed observation was refused: %v", method, err)
			}
		case Unclassified:
			t.Errorf("%s: classified as Unclassified", method)
		}
	}
	// Both arms have to be populated or this test proves one branch only. The
	// numbers are not pinned — a new RPC moves them — but zero on either side
	// means the walk stopped covering a case.
	if effects == 0 || observations == 0 {
		t.Fatalf("the walk covered %d effects and %d observations", effects, observations)
	}
}

// A capability that does not parse is a bad request, not an absent one — for
// an observation too, where the rule otherwise permits.
func TestAMalformedCapabilityIsRefusedRatherThanReadAsAbsent(t *testing.T) {
	ctx := contextCarrying(t, "not-a-capability")
	for _, method := range []string{"ReadFile", "WriteFile"} {
		if err := Admit(ctx, method); err == nil {
			t.Errorf("%s: a malformed capability was read as absent", method)
		}
	}
}

// Nothing outside pkg/gateway reaches an effect by calling it.
//
// The boundary is a transport interceptor, so it covers every gRPC request,
// unary and streaming. It does NOT cover a caller that holds a *gateway.Server
// and invokes a method on it in process — and the fix for B1 was asked for
// across "every transport, including direct calls", so that gap deserves an
// assertion rather than a paragraph of reasoning.
//
// Today the gap has no caller. Exactly two non-test files outside pkg/gateway
// import the package: cmd/daemon.go, which calls Serve and nothing else, and
// pkg/gateway/dockerexec/shell.go, which uses one free function and never holds
// a Server. This test fails the moment that changes, so whoever adds the first
// in-process caller has to decide what admits it instead of inheriting a
// boundary that does not reach them.
//
// What it proves: no file outside pkg/gateway that imports pkg/gateway names an
// effect method in a call. What it does not prove: anything about reflection,
// or about a caller that reaches the server through an interface it was handed
// — neither of which exists here, and both of which would also defeat an
// in-body check.
func TestNoPackageOutsideTheGatewayCallsAnEffectDirectly(t *testing.T) {
	const gatewayImport = `"github.com/codefly-dev/cli/pkg/gateway"`
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	effects := map[string]bool{}
	for _, method := range Methods() {
		if class, _ := ClassOf(method); class == Effect {
			effects[method] = true
		}
	}
	if len(effects) == 0 {
		t.Fatal("no effect methods in the inventory, so this test proves nothing")
	}

	var offenders []string
	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "vendor", "testdata", "node_modules":
				return filepath.SkipDir
			}
			// pkg/gateway owns this boundary and calls Admit inside itself;
			// its own internal calls are what the interceptor and dockerexec's
			// in-body admits already cover.
			if path == filepath.Join(root, "pkg", "gateway") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		source, readErr := os.ReadFile(path) // #nosec G304 -- a .go file inside this module
		if readErr != nil {
			return readErr
		}
		if !strings.Contains(string(source), gatewayImport) {
			return nil
		}
		file, parseErr := parser.ParseFile(token.NewFileSet(), path, source, 0)
		if parseErr != nil {
			return parseErr
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if effects[selector.Sel.Name] {
				relative, _ := filepath.Rel(root, path)
				offenders = append(offenders, fmt.Sprintf("%s calls %s", relative, selector.Sel.Name))
			}
			return true
		})
		return nil
	})
	if walkErr != nil {
		t.Fatal(walkErr)
	}
	if len(offenders) > 0 {
		t.Fatalf("an effect is reached in process, where the transport boundary does not apply:\n  %s\n"+
			"route it through effect.Admit, or give the caller its own admitted path",
			strings.Join(offenders, "\n  "))
	}
}
