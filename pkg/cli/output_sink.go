package cli

import (
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/wool"
)

// flowOutputSink adapts the package-level CLI narration functions to
// orchestration.OutputSink, so cobra commands can keep their existing
// stdout narration now that pkg/orchestration no longer imports pkg/cli
// directly.
type flowOutputSink struct{}

// NewOutputSink returns the pkg/cli-backed orchestration.OutputSink.
func NewOutputSink() orchestration.OutputSink {
	return flowOutputSink{}
}

func (flowOutputSink) Process(log *wool.Log) { GetLogger().Process(log) }

func (flowOutputSink) ProcessWithSource(source *wool.Identifier, log *wool.Log) {
	GetLogger().ProcessWithSource(source, log)
}

func (flowOutputSink) Info(format string, args ...any) { Info(format, args...) }

func (flowOutputSink) Error(format string, args ...any) { Error(format, args...) }

func (flowOutputSink) RegisterLoggingResource(unique string) { RegisterLoggingResource(unique) }
