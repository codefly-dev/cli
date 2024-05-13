package test

// Scope for the runtime: affect ports to avoid conflict with run
// Useful for testing/CI
var scope string

// Runtime context
var runtimeContext string

// Run in CI mode
var ci bool

// load only mode
var loadOnly bool

// init only mode
var initOnly bool
