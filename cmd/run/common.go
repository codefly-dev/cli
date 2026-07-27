package run

// Run codefly frontend companion
var withCLIServer bool

// Headless mode: no TUI, plain log output (auto-enabled when no TTY)
var headless bool

// Path where to find the service
var servicePath string

// Output environment variables
var outputEnv string

// Remote services
var remotes []string

// Silent services in the CLI
var silent []string

// Services to omit from the dependency graph for this run.
var excludeDependencies []string

// With fixture across the runtime
var fixture string

// Per-service runtime overrides: each entry is "service:KEY=VAL".
var setOverrides []string

// Scope for the runtime: affect ports to avoid conflict with run
// Useful for testing/CI
var namingScope string

// namingScopeExplicit records whether --naming-scope was passed at all, so an
// explicit empty value can clear a workspace-declared scope while an absent
// flag keeps it.
var namingScopeExplicit bool

// temporaryPorts asks the runtime allocator for OS-probed ephemeral ports.
// The Codefly SDK enables this for test-owned dependency stacks so independent
// package test processes cannot collide through the deterministic port hash.
var temporaryPorts bool

// Runtime context
var runtimeContext string

// Don't run dependencies
var standAlone bool

// Only run dependencies
var excludeRoot bool

// load only mode
var loadOnly bool

// init only mode
var initOnly bool

// startDocker auto-starts a local Docker engine (OrbStack/Docker Desktop/colima/…)
// when a service can only run under Docker and the daemon is not reachable.
var startDocker bool
