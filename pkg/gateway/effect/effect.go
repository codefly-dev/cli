// Package effect is the one admission boundary for every Codefly Gateway
// effect, whatever transport reaches it.
//
// It exists because the governed-execution gate was per-call-site. Three
// methods — ApplyEdit, ApplySymbolPatch and Test — asked whether the request
// carried a Work Context and refused when it did; the other fifty-four served
// the request and performed the effect, with the capability in the metadata
// IGNORED. A caller presenting a capability to WriteFile got the write, no
// admission and no receipt, which is worse than refusing it: presenting a
// capability is the caller stating that this execution is governed, and
// silently performing it ungoverned answers that statement with a lie.
//
// A per-call-site gate cannot be fixed by adding the missing call sites,
// because the next RPC starts ungated again. So the rule lives here, once, and
// reaches the methods through a transport interceptor rather than through
// fifty-seven remembered calls. ClassOf refuses a method it has never heard
// of, so an RPC added to the contract without declaring what it does is
// refused rather than served — and MethodsCoverTheContract asserts the table
// against the generated service descriptor, so that refusal surfaces at test
// time instead of in production.
//
// # Where the boundary is, and where it is not
//
// Every gRPC request — unary and streaming — passes through UnaryInterceptor
// and StreamInterceptor, installed once where the server is registered. The
// container executor (pkg/gateway/dockerexec), a second implementation of the
// same contract, calls Admit on its own effect path. The recorder's
// begin-governed path calls RefuseGoverned, so there is one refusal and one
// message rather than two that can drift.
//
// It is NOT inside each method body, and that is deliberate rather than
// unfinished. A capability travels in request metadata, so a caller that
// presents one is remote by construction; the gateway's own code calling its
// own method in-process is already inside the trust boundary and could skip an
// in-body check as easily as the interceptor. Putting the rule in fifty-seven
// bodies would restore exactly the per-call-site arrangement this package
// replaces, and the mechanical proof — walking the contract — would be
// replaced by an AST test asserting the shape of a function's first statement.
package effect

import (
	"context"
	"fmt"
	"sort"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	workcontext "github.com/codefly-dev/sdk-go/workcontext"
	workcontextgrpc "github.com/codefly-dev/sdk-go/workcontext/grpctransport"
)

// Class is what one Gateway method does to the world.
type Class int

const (
	// Unclassified is the zero value, and no method has it: ClassOf reports a
	// method it does not know as unknown rather than returning this. It is
	// here so that a zero Class cannot be mistaken for a permission.
	Unclassified Class = iota

	// Observation reads. It writes no file, starts no process, reaches no
	// forge and leaves nothing behind, so there is no effect to record and a
	// capability presented alongside it authorizes nothing that needs
	// authorizing.
	Observation

	// Effect changes something: the workspace, a process, a repository, or
	// state in a remote system. These are the methods a receipt exists for.
	Effect
)

func (c Class) String() string {
	switch c {
	case Observation:
		return "observation"
	case Effect:
		return "effect"
	default:
		return "unclassified"
	}
}

// classes is the whole inventory of the Gateway contract.
//
// Keyed by the method's own name, not its full path, because both
// implementations of the contract serve the same names.
//
// The line is drawn at what a method is ENTITLED to do, not at where the work
// happens. Build, Lint, Test, Format, RunCommand and RunChecks run the
// toolchain or an arbitrary command, whose side effects are unbounded, so they
// are effects even when a particular run happens to write nothing.
// ResizeTerminal and CloseTerminal act on a live process. PrepareMutation and
// MaterializeRepositorySnapshot leave durable state on disk even though
// neither is the final write. ForgeMergePullRequest and ForgeRequestReview
// reach outside the machine entirely.
//
// Dispatching to the service agent is NOT by itself what makes an effect, and
// an earlier draft of this comment said it was. GitDiff's unstaged path,
// GetSourceManifest and DiscoverCodeUnits go through the same agent transport
// as Lint, and they are observations: the contract asks each of them for a
// reading and for nothing else. That makes them reads on the agent's word —
// the CLI cannot verify what a third-party agent does inside either kind of
// call, so the honest distinction is what the contract asks for, not where it
// is executed. GitStatus is a read in the same spirit: `git status` may
// refresh the index stat cache while answering, which changes no tracked
// content, no history and no ref, and leaves nothing a receipt could attest
// to.
var classes = map[string]Class{
	// Reads.
	"ListServices":            Observation,
	"EvaluateStorageCapacity": Observation,
	"ReadFile":                Observation,
	"ListFiles":               Observation,
	"Search":                  Observation,
	// These four are READS whose answer comes from the service's language
	// agent, and obtaining the agent STARTS it. A static call-graph guard
	// (TestNoObservationReachesTheAgentDoor) found them after r16 had executed
	// three others: GitDiff's unstaged path, GetSourceManifest and
	// DiscoverCodeUnits reach the door through sourceExecute, GetSemanticIndex
	// through rootedSourceExecute -> boundSource.
	//
	// This NARROWS the owner's ruling that a read is not an effect, and only
	// for these: the effect is not the reading, it is starting a process to
	// perform it. GitStatus, GitLog, GitDiff's staged path,
	// ForgePullRequestStatus and ForgeNormalizeWebhook remain observations —
	// they run in-process git or a pure transform and reach no door.
	"GetSourceManifest": Effect,
	"DiscoverCodeUnits": Effect,

	// Reading metadata STARTS the service's language agent
	// (AgentSupervisor.acquire). r16 executed the consequence: governed Build
	// was refused at the transport boundary while governed ListAllCommands
	// spawned go__0.0.63-dev. Starting a process is an effect whatever the
	// answer is used for — and ListAllCommands IGNORES the error from
	// executionServiceBehavior, so a refusal deeper down would otherwise be
	// swallowed into a response quietly missing its agent commands.
	"ListAllCommands":           Effect,
	"GitStatus":                 Observation,
	"GitDiff":                   Effect,
	"GitLog":                    Observation,
	"ForgePullRequestStatus":    Observation,
	"ForgeNormalizeWebhook":     Observation,
	"ListDependencies":          Effect,
	"GetProjectInfo":            Effect,
	"GetSemanticIndex":          Effect,
	"GetInstructionIndex":       Observation,
	"ListTerminals":             Observation,
	"SubscribeWorkspaceChanges": Observation,

	// Writes to the workspace.
	"WriteFile":        Effect,
	"DeleteFile":       Effect,
	"MoveFile":         Effect,
	"CreateFile":       Effect,
	"Fix":              Effect,
	"ApplyEdit":        Effect,
	"ApplySymbolPatch": Effect,
	"BatchApplyEdits":  Effect,
	"Format":           Effect,
	"ApplyPatch":       Effect,

	// Standing configuration and prepared mutation.
	"ConfigureMutationAuthority": Effect,
	"PrepareMutation":            Effect,
	"ApplyPreparedMutation":      Effect,
	"ConfigureService":           Effect,
	"AddDependency":              Effect,
	"RemoveDependency":           Effect,

	// Execution of code in the workspace.
	"Build":      Effect,
	"Lint":       Effect,
	"Test":       Effect,
	"RunCommand": Effect,
	"RunChecks":  Effect,

	// Repository history.
	"GitCommit":   Effect,
	"GitBranch":   Effect,
	"GitCheckout": Effect,
	"GitPush":     Effect,
	"GitTag":      Effect,
	"GitMerge":    Effect,
	"GitRevert":   Effect,

	// Durable repository state and release.
	"MaterializeRepositorySnapshot": Effect,
	"PrepareRepositoryCheckout":     Effect,
	"ReleaseRepositorySnapshot":     Effect,
	"Release":                       Effect,

	// Outside this machine.
	"ForgeMergePullRequest": Effect,
	"ForgeRequestReview":    Effect,

	// Live processes.
	"OpenTerminal":   Effect,
	"ResizeTerminal": Effect,
	"CloseTerminal":  Effect,
	"AttachTerminal": Effect,
}

// ClassOf reports what a method does. The second result is false for a method
// the inventory does not name, which Admit refuses: the gateway cannot decide
// whether an undeclared method needs admission, and guessing is how an effect
// gets served ungoverned.
func ClassOf(method string) (Class, bool) {
	class, ok := classes[MethodName(method)]
	return class, ok
}

// Methods lists the inventory, sorted, for the test that walks the contract.
func Methods() []string {
	names := make([]string, 0, len(classes))
	for name := range classes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// MethodName takes the method's own name out of a gRPC full method path
// ("/mind.gateway.v1.Gateway/WriteFile" becomes "WriteFile") and leaves a bare
// name alone, so an in-process caller and an interceptor name the same method
// the same way.
func MethodName(fullMethod string) string {
	for index := len(fullMethod) - 1; index >= 0; index-- {
		if fullMethod[index] == '/' {
			return fullMethod[index+1:]
		}
	}
	return fullMethod
}

// Carried reports whether the request presents a Work Context capability.
//
// Presenting one is the caller stating that this execution is governed. It is
// the only thing that turns an ordinary request into a governed one, so it is
// read in exactly one place.
func Carried(ctx context.Context) bool {
	values, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return false
	}
	return len(values.Get(workcontext.HeaderName)) > 0
}

// Admit is the rule, and the only one.
//
//  1. A method the inventory does not name is refused. Fail closed: a new RPC
//     is declared before it serves anything.
//  2. A request carrying no capability is not a governed request. It proceeds
//     exactly as it did before this package existed — refusing it would turn
//     every ordinary Gateway call into an error.
//  3. A malformed capability is refused as a bad request, before anything
//     reads it as absent.
//  4. A governed EFFECT is refused, because this process cannot admit one.
//     See RefuseGoverned.
//  5. A governed OBSERVATION proceeds. It records nothing, governed or not, so
//     refusing it would deny every read to a governed caller and protect
//     nothing: there is no effect to leave unrecorded.
func Admit(ctx context.Context, fullMethod string) error {
	method := MethodName(fullMethod)
	class, known := ClassOf(method)
	if !known {
		return status.Errorf(
			codes.Internal,
			"Codefly Gateway method %q declares no effect class: it is refused until pkg/gateway/effect names it, because the gateway cannot tell whether it performs an effect that would need admission",
			method,
		)
	}
	if !Carried(ctx) {
		return nil
	}
	if _, err := workcontextgrpc.GRPCExecutionContextFromIncoming(ctx); err != nil {
		return status.Errorf(codes.InvalidArgument, "invalid Codefly execution context: %v", err)
	}
	if class != Effect {
		return nil
	}
	return RefuseGoverned()
}

// RefuseGoverned is the single refusal for a governed effect.
//
// REFUSED, not admitted. This process holds none of the four live sources
// core's Verifier requires — the authorization revision, the replay store, the
// grant source, the seal source — so it cannot turn the presented capability
// into a *workcontext.Verified, and it will not pretend to. A verifier fed
// invented state does not fail, it PASSES, which is precisely what the seal
// check exists to prevent.
//
// Governed execution becomes servable when the component holding those sources
// verifies the capability and hands the recorder a *Verified, which its
// Authority already takes and refuses when nil. That is a change to this
// function, not a seam someone has to find.
func RefuseGoverned() error {
	return status.Error(
		codes.Unimplemented,
		"governed execution requires a verified Work Context: this gateway does not hold the authorization-revision, replay, grant and seal sources needed to verify one, and will not admit an unverified capability",
	)
}

// UnaryInterceptor admits every unary request before its handler runs.
func UnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := Admit(ctx, info.FullMethod); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// StreamInterceptor admits every stream before its handler runs. A stream is
// where a refusal after the fact is worth least: AttachTerminal writes the
// caller's bytes into a live process, so the whole effect happens inside the
// handler.
func StreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := Admit(stream.Context(), info.FullMethod); err != nil {
			return err
		}
		return handler(srv, stream)
	}
}

// Describe is for diagnostics: the inventory as "method: class" lines.
func Describe() string {
	out := ""
	for _, name := range Methods() {
		out += fmt.Sprintf("%s: %s\n", name, classes[name])
	}
	return out
}
