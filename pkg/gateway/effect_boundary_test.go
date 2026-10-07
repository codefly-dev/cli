package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	gatewayv1 "github.com/codefly-dev/core/generated/go/mind/gateway/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/codefly-dev/cli/pkg/gateway/effect"
	workcontextgrpc "github.com/codefly-dev/sdk-go/workcontext/grpctransport"
)

// Every effect in the contract is refused before its handler runs.
//
// The walk is over the GENERATED service descriptor, so a method added to the
// contract is covered the moment it exists — the thing a hand-written list of
// gated methods could never promise. Each entry goes through the generated
// handler with the real interceptor, which is how a served request reaches a
// method, and the handler the interceptor would call is wrapped so that
// "refused before any effect" is observed rather than assumed.
//
// Before this boundary existed, three methods asked about the capability and
// fifty-four ignored it: this test fails for every one of those fifty-four if
// the interceptor is removed.
func TestEveryEffectOfTheContractIsRefusedBeforeItsHandler(t *testing.T) {
	root := t.TempDir()
	seedWorkspace(t, root)
	before := workspaceDigest(t, root)

	server, err := NewServer(&Config{WorkDir: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	governed := incomingExecutionContext(t, "operation-effect-boundary")
	descriptor := gatewayv1.Gateway_ServiceDesc
	walked := 0

	for _, method := range descriptor.Methods {
		class, known := effect.ClassOf(method.MethodName)
		if !known {
			t.Errorf("%s: no effect class (pkg/gateway/effect must name it)", method.MethodName)
			continue
		}
		if class != effect.Effect {
			continue
		}
		walked++
		reached := false
		spy := func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			return effect.UnaryInterceptor()(ctx, req, info, func(inner context.Context, message any) (any, error) {
				reached = true
				return handler(inner, message)
			})
		}
		_, err := method.Handler(server, governed, leaveRequestZero, spy)
		if reached {
			t.Errorf("%s: the handler ran for a governed request", method.MethodName)
		}
		requireGovernedRefusal(t, method.MethodName, err)
	}

	for _, stream := range descriptor.Streams {
		class, known := effect.ClassOf(stream.StreamName)
		if !known {
			t.Errorf("%s: no effect class (pkg/gateway/effect must name it)", stream.StreamName)
			continue
		}
		if class != effect.Effect {
			continue
		}
		walked++
		reached := false
		info := &grpc.StreamServerInfo{
			FullMethod:     "/" + descriptor.ServiceName + "/" + stream.StreamName,
			IsClientStream: stream.ClientStreams,
			IsServerStream: stream.ServerStreams,
		}
		err := effect.StreamInterceptor()(server, governedStream{ctx: governed}, info, func(any, grpc.ServerStream) error {
			reached = true
			return nil
		})
		if reached {
			t.Errorf("%s: the stream handler ran for a governed request", stream.StreamName)
		}
		requireGovernedRefusal(t, stream.StreamName, err)
	}

	if walked < 2 {
		t.Fatalf("the walk covered %d effects, so it is not walking the contract", walked)
	}
	if after := workspaceDigest(t, root); after != before {
		t.Fatalf("a refused walk changed the workspace:\nbefore %s\nafter  %s", before, after)
	}
}

// The chain a real client goes through refuses a governed effect, and leaves
// an ungoverned one alone.
//
// This is the end-to-end half: serverOptions() is the configuration Serve
// installs, so a refusal here is a refusal on the wire rather than a property
// of a chain assembled by the test.
func TestTheServedChainRefusesAGovernedEffect(t *testing.T) {
	root := t.TempDir()
	server, err := NewServer(&Config{WorkDir: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer(server.serverOptions()...)
	gatewayv1.RegisterGatewayServer(grpcServer, server)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)

	connection, err := grpc.NewClient("passthrough:///effect-boundary",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return listener.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	client := gatewayv1.NewGatewayClient(connection)

	write := &gatewayv1.WriteFileRequest{Path: "governed.txt", Content: "must not be written"}
	if _, err := client.WriteFile(outgoingExecutionContext(t, "operation-served-chain"), write); err == nil {
		t.Fatal("the served chain wrote a file for a governed request")
	} else {
		requireGovernedRefusal(t, "WriteFile", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "governed.txt")); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("a refused governed write left a file behind: %v", statErr)
	}

	// The same method, ungoverned, is served: the boundary refuses a governed
	// effect, not every effect. Without this the test would also pass with a
	// gateway that refuses everything.
	if _, err := client.WriteFile(t.Context(), write); err != nil {
		t.Fatalf("an ungoverned write was refused: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "governed.txt")); statErr != nil {
		t.Fatalf("an admitted write produced no file: %v", statErr)
	}

	// The STREAM half of the served chain. Without this, deleting
	// effect.StreamInterceptor() from serverOptions breaks nothing: the walk
	// above calls that interceptor directly, so only a real stream over the
	// wire proves it is installed.
	attach, err := client.AttachTerminal(outgoingExecutionContext(t, "operation-served-stream"))
	if err != nil {
		t.Fatal(err)
	}
	if sendErr := attach.Send(&gatewayv1.TerminalInput{}); sendErr != nil && !errors.Is(sendErr, io.EOF) {
		t.Fatalf("send on a refused stream = %v", sendErr)
	}
	if _, recvErr := attach.Recv(); recvErr == nil {
		t.Fatal("the served chain opened a governed terminal stream")
	} else {
		requireGovernedRefusal(t, "AttachTerminal", recvErr)
	}

	// And a governed OBSERVATION is served, for the same reason.
	if _, err := client.ReadFile(
		outgoingExecutionContext(t, "operation-served-chain-read"),
		&gatewayv1.ReadFileRequest{Path: "governed.txt"},
	); err != nil {
		t.Fatalf("a governed observation was refused: %v", err)
	}
}

func requireGovernedRefusal(t *testing.T, method string, err error) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: a governed effect was admitted", method)
		return
	}
	if code := status.Code(err); code != codes.Unimplemented {
		t.Errorf("%s: refusal code = %s, want Unimplemented: %v", method, code, err)
	}
	if !strings.Contains(err.Error(), "requires a verified Work Context") {
		t.Errorf("%s: the refusal must say what is missing: %v", method, err)
	}
}

// leaveRequestZero is the generated handler's decoder. It leaves the request
// at its zero value on purpose: a refusal that depends on the request's
// contents is not a boundary.
func leaveRequestZero(any) error { return nil }

type governedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s governedStream) Context() context.Context { return s.ctx }
func (s governedStream) SendMsg(any) error        { return errors.New("the stream must not be reached") }
func (s governedStream) RecvMsg(any) error        { return io.EOF }

func outgoingExecutionContext(t *testing.T, operationID string) context.Context {
	t.Helper()
	execution, err := workcontextgrpc.NewExecutionContext(conformanceCarrier(t), operationID)
	if err != nil {
		t.Fatal(err)
	}
	outgoing, err := workcontextgrpc.WithGRPCExecutionContext(t.Context(), execution)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := metadata.FromOutgoingContext(outgoing); !ok {
		t.Fatal("the SDK attached no outgoing execution metadata")
	}
	return outgoing
}

func seedWorkspace(t *testing.T, root string) {
	t.Helper()
	for name, content := range map[string]string{
		"main.go":     "package main\n\nfunc main() {}\n",
		"README.md":   "# workspace\n",
		"nested/a.go": "package nested\n",
	} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// workspaceDigest is every path under root with its content digest, so "no
// effect" is a comparison rather than a spot check: a created, deleted, moved
// or edited file all change it.
func workspaceDigest(t *testing.T, root string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if entry.IsDir() {
			lines = append(lines, "dir "+relative)
			return nil
		}
		content, readErr := os.ReadFile(path) // #nosec G304 -- a path this test just wrote under t.TempDir()
		if readErr != nil {
			return readErr
		}
		digest := sha256.Sum256(content)
		lines = append(lines, "file "+relative+" "+hex.EncodeToString(digest[:]))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}
