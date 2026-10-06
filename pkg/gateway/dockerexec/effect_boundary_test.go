package dockerexec

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"reflect"
	"strings"
	"testing"
	"time"

	coreworkcontext "github.com/codefly-dev/core/workcontext"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/codefly-dev/cli/pkg/gateway/effect"
	workcontextgrpc "github.com/codefly-dev/sdk-go/workcontext/grpctransport"
)

// Every effect this executor implements refuses a governed request.
//
// This is the second implementation of the Gateway contract, and it is reached
// without an interceptor, so the boundary is called in the method bodies. The
// list of methods to check is read from THIS PACKAGE'S SOURCE rather than
// written down: a new effect method added to the executor appears here on its
// own, and fails until it admits.
//
// The methods are invoked through reflection with a zero request, so the
// refusal cannot depend on the request's contents, and a zero-value *Gateway
// is enough — no Docker, no container — precisely because the boundary comes
// before any of that. A method that reaches the executor instead panics on the
// nil base, which this test reports as a failure rather than a pass.
func TestEveryImplementedEffectRefusesAGovernedRequest(t *testing.T) {
	governed := governedContext(t)
	gateway := &Gateway{}
	value := reflect.ValueOf(gateway)
	checked := 0

	for _, method := range declaredMethods(t) {
		class, known := effect.ClassOf(method)
		if !known || class != effect.Effect {
			continue
		}
		callable := value.MethodByName(method)
		if !callable.IsValid() || callable.Type().NumIn() != 2 {
			continue
		}
		requestType := callable.Type().In(1)
		if requestType.Kind() != reflect.Pointer {
			continue
		}
		checked++
		t.Run(method, func(t *testing.T) {
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Fatalf("the method ran past the boundary and panicked on an unconfigured executor: %v", recovered)
				}
			}()
			results := callable.Call([]reflect.Value{
				reflect.ValueOf(governed),
				reflect.New(requestType.Elem()),
			})
			last := results[len(results)-1]
			if last.IsNil() {
				t.Fatal("a governed effect was admitted")
			}
			err, ok := last.Interface().(error)
			if !ok {
				t.Fatalf("last result is not an error: %T", last.Interface())
			}
			if code := status.Code(err); code != codes.Unimplemented {
				t.Fatalf("refusal code = %s, want Unimplemented: %v", code, err)
			}
			if !strings.Contains(err.Error(), "requires a verified Work Context") {
				t.Fatalf("the refusal must say what is missing: %v", err)
			}
		})
	}

	if checked == 0 {
		t.Fatal("no implemented effect was checked, so this test proves nothing")
	}
}

// declaredMethods are the methods this package declares on *Gateway. Promoted
// ones from the embedded UnimplementedGatewayServer are excluded by reading the
// source: they serve nothing, so requiring them to admit would assert against
// generated code instead of against this executor.
func declaredMethods(t *testing.T) []string {
	t.Helper()
	fileSet := token.NewFileSet()
	packages, err := parser.ParseDir(fileSet, ".", func(info fs.FileInfo) bool {
		return !strings.HasSuffix(info.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, parsed := range packages {
		for _, file := range parsed.Files {
			for _, declaration := range file.Decls {
				function, ok := declaration.(*ast.FuncDecl)
				if !ok || function.Recv == nil || len(function.Recv.List) != 1 {
					continue
				}
				pointer, ok := function.Recv.List[0].Type.(*ast.StarExpr)
				if !ok {
					continue
				}
				identifier, ok := pointer.X.(*ast.Ident)
				if !ok || identifier.Name != "Gateway" {
					continue
				}
				if function.Name.IsExported() {
					names = append(names, function.Name.Name)
				}
			}
		}
	}
	if len(names) == 0 {
		t.Fatal("no exported *Gateway method was found in this package's source")
	}
	return names
}

func governedContext(t *testing.T) context.Context {
	t.Helper()
	fixtures, err := coreworkcontext.Fixtures(time.Date(2026, time.July, 23, 19, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	token := ""
	for _, fixture := range fixtures {
		if fixture.Outcome == coreworkcontext.OutcomeAccepted && fixture.Token != "" {
			token = fixture.Token
			break
		}
	}
	if token == "" {
		t.Fatal("core's conformance kit produced no accepted capability carrier")
	}
	execution, err := workcontextgrpc.NewExecutionContext(token, "operation-dockerexec-boundary")
	if err != nil {
		t.Fatal(err)
	}
	outgoing, err := workcontextgrpc.WithGRPCExecutionContext(context.Background(), execution)
	if err != nil {
		t.Fatal(err)
	}
	carrier, ok := metadata.FromOutgoingContext(outgoing)
	if !ok {
		t.Fatal("the SDK attached no outgoing execution metadata")
	}
	return metadata.NewIncomingContext(context.Background(), carrier.Copy())
}
