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
