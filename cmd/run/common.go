package run

// Run codefly frontend companion
var withCLIServer bool

// Silent services in the CLI
var silent []string

// Scope for the runtime: affect ports to avoid conflict with run
// Useful for testing/CI
var scope string

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
