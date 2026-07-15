package test

// Scope for the runtime: affect ports to avoid conflict with run
// Useful for testing/CI
var scope string

// Runtime context
var runtimeContext string

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
