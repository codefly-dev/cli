package gateway

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"sort"
	"strings"
	"testing"

	gatewayv1 "github.com/codefly-dev/core/generated/go/mind/gateway/v1"

	"github.com/codefly-dev/cli/pkg/gateway/effect"
)

// No observation can reach the code that starts a language agent.
//
// Starting a process is an effect, and r16 found three RPCs classed as
// observations that reached it: a governed caller had Build refused at the
// transport boundary while ListAllCommands spawned go__0.0.63-dev. The refusal
// now lives at the funnel (engine.AgentSupervisor.acquire), and the three are
// classed Effect.
//
// This test exists because NEITHER of those fixes is self-policing, and
// because a behavioural walk could not police them: the funnel is only reached
// when a workspace resolves an agent, which no unit-test environment does, so a
// served-chain walk reported no offender while r16 had executed three. Worse,
// callers swallow the refusal — ListAllCommands proceeds `if err == nil` — so
// even a reachable walk could miss it.
//
// So reachability is read from the SOURCE instead. The doors are the gateway
// functions that obtain an engine handle (h.Service, h.Source, h.SourceAt);
// every operation on one goes through engine.Service.withSession, which calls
// acquire. Any Gateway RPC that transitively calls a door can start a process
// and must be classed Effect.
//
// This found four MORE than r16's execution did — GitDiff, GetSemanticIndex,
// GetSourceManifest and DiscoverCodeUnits, each reaching a door through
// sourceExecute or boundSource — because an execution only exercises the paths
// its workspace happens to take, while the call graph carries all of them.
func TestNoObservationReachesTheAgentDoor(t *testing.T) {
	graph, doors := gatewayCallGraph(t)
	if len(doors) == 0 {
		t.Fatal("no door was found, so this test proves nothing: has the engine handle been renamed?")
	}

	var offenders []string
	for _, method := range gatewayv1.Gateway_ServiceDesc.Methods {
		class, known := effect.ClassOf(method.MethodName)
		if !known || class != effect.Observation {
			continue
		}
		if path := pathToDoor(graph, doors, method.MethodName); path != nil {
			offenders = append(offenders, fmt.Sprintf("%s (via %s)", method.MethodName, strings.Join(path, " -> ")))
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("these observations can start a language agent:\n  %s\n"+
			"starting a process is an effect: class them Effect in pkg/gateway/effect, "+
			"or route them through something that does not acquire an agent",
			strings.Join(offenders, "\n  "))
	}
}

// gatewayCallGraph is this package's intra-package call graph by function name,
// plus the set of functions that obtain an engine handle.
//
// Names rather than types: a selector's receiver is not resolved, so two
// methods sharing a name share a node. That over-approximates reachability,
// which is the safe direction for this question — a false positive is a
// classification argument, a false negative is an ungated process start.
func gatewayCallGraph(t *testing.T) (map[string]map[string]bool, map[string]bool) {
	t.Helper()
	fileSet := token.NewFileSet()
	packages, err := parser.ParseDir(fileSet, ".", func(info fs.FileInfo) bool {
		return !strings.HasSuffix(info.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	graph := map[string]map[string]bool{}
	doors := map[string]bool{}
	for _, parsed := range packages {
		for _, file := range parsed.Files {
			for _, declaration := range file.Decls {
				function, ok := declaration.(*ast.FuncDecl)
				if !ok {
					continue
				}
				name := function.Name.Name
				if graph[name] == nil {
					graph[name] = map[string]bool{}
				}
				reached := ""
				ast.Inspect(function, func(node ast.Node) bool {
					switch value := node.(type) {
					case *ast.SelectorExpr:
						graph[name][value.Sel.Name] = true
						switch inner := value.X.(type) {
						case *ast.Ident:
							reached += inner.Name + "." + value.Sel.Name + " "
						case *ast.SelectorExpr:
							reached += inner.Sel.Name + "." + value.Sel.Name + " "
						}
					case *ast.Ident:
						graph[name][value.Name] = true
					}
					return true
				})
				for _, door := range []string{"host.Service", "host.Source", "host.SourceAt"} {
					if strings.Contains(reached, door) {
						doors[name] = true
					}
				}
			}
		}
	}
	return graph, doors
}

// pathToDoor is the first route from start to a door, or nil.
func pathToDoor(graph map[string]map[string]bool, doors map[string]bool, start string) []string {
	seen := map[string]bool{}
	var found []string
	var walk func(string, []string) bool
	walk = func(node string, trail []string) bool {
		if seen[node] {
			return false
		}
		seen[node] = true
		if doors[node] {
			found = append(trail, node)
			return true
		}
		called := make([]string, 0, len(graph[node]))
		for name := range graph[node] {
			called = append(called, name)
		}
		sort.Strings(called)
		for _, name := range called {
			if _, defined := graph[name]; defined && walk(name, append(trail, node)) {
				return true
			}
		}
		return false
	}
	if walk(start, nil) {
		return found
	}
	return nil
}
