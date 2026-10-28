package cli

import (
	"github.com/codefly-dev/core/wool"
)

// FlowOutputSink adapts the package-level CLI narration functions to
// orchestration.OutputSink, so cobra commands can keep their existing
// stdout narration now that pkg/orchestration no longer imports pkg/cli
// directly.
//
// It satisfies that interface structurally and does not name it, which is
// what keeps pkg/cli BELOW pkg/orchestration in the import graph: naming it
// put pkg/cli above orchestration, and since pkg/composition narrates through
// pkg/cli, orchestration could then not read a composed module's tree
// (module_artifact.go) without a cycle.
type FlowOutputSink struct{}

// NewOutputSink returns the pkg/cli-backed orchestration.OutputSink.
func NewOutputSink() FlowOutputSink {
	return FlowOutputSink{}
}

func (FlowOutputSink) Process(log *wool.Log) { GetLogger().Process(log) }

func (FlowOutputSink) ProcessWithSource(source *wool.Identifier, log *wool.Log) {
	GetLogger().ProcessWithSource(source, log)
}

func (FlowOutputSink) Info(format string, args ...any) { Info(format, args...) }

func (FlowOutputSink) Error(format string, args ...any) { Error(format, args...) }

func (FlowOutputSink) RegisterLoggingResource(unique string) { RegisterLoggingResource(unique) }
