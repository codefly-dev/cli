package test

// Runtime context
var runtimeContext string

// Fixture selected for dependency-backed tests. Empty means use the fixture
// declared by the selected Codefly environment.
var testFixture string

// Workspace environment the test flow runs in. A test reaches an
// environment-declared fixture only by selecting that environment, so this is
// a flag rather than a hardcoded local.
var environmentName string

// Named workspace run profile, and the extra dependency services to omit on
// top of it.
var (
	profile             string
	excludeDependencies []string
)

// Output environment variables
var outputEnv string

// Runtime naming scope: folded into port derivation and every resource name
// the flow owns. namingScopeExplicit records whether the flag was passed at
// all, so an explicit empty value can clear a workspace-declared scope (and
// opt out of the generated one) while an absent flag keeps it.
var (
	namingScope         string
	namingScopeExplicit bool
)

// temporaryPorts marks the flow as a disposable invocation: OS-probed
// ephemeral ports plus a generated naming scope for every other resource it
// owns. On by default here — two `codefly test service` invocations in one
// workspace are independent by construction, not by the operator remembering
// to isolate them.
var temporaryPorts bool

// pinsAlreadyResolved records that the caller materialized this workspace's
// composed pinned modules before delegating to the test-service path, so that
// path does not repeat it: `test solution` materializes unconditionally, and a
// second pass would re-attempt a pull that already failed in the same
// invocation. Set and cleared around the delegation, so a repeated in-process
// invocation cannot inherit it.
var pinsAlreadyResolved bool

// load only mode
var loadOnly bool

// init only mode
var initOnly bool

// headless mode
var headless bool

// Test filtering — forwarded to the agent's Test RPC.
var (
	// testTarget — package or directory scope.
	testTarget string
	// testFilters — name regex patterns (repeatable, OR-combined).
	testFilters []string
	// testSuite — named suite (unit/integration/e2e/smoke).
	testSuite string
	// testTimeout — per-test timeout, e.g. "30s".
	testTimeout string
	// testVerbose — verbose runner output.
	testVerbose bool
	// testRace — Go race detector.
	testRace bool
	// testCoverage — coverage instrumentation.
	testCoverage bool
)
