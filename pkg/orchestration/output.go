package orchestration

import "github.com/codefly-dev/core/wool"

// OutputSink receives the narration and structured logs a Flow would
// otherwise print directly via pkg/cli, so an embedder controls where output
// goes instead of writing to stdout. It also serves as the log processor
// handed to core's RemoteNetworkManager.Expose. Set via WithOutputSink,
// mirroring the WithStateListener pattern; unset, output is silently
// discarded. The cobra command injects the pkg/cli-backed implementation at
// its call site to keep `codefly run`'s existing behavior.
type OutputSink interface {
	wool.LogProcessor
	wool.LogProcessorWithSource
	Info(format string, args ...any)
	Error(format string, args ...any)
	// RegisterLoggingResource tells the sink about a service name it should
	// account for when aligning streamed log output.
	RegisterLoggingResource(unique string)
}

type noopOutputSink struct{}

func (noopOutputSink) Process(*wool.Log)                             {}
func (noopOutputSink) ProcessWithSource(*wool.Identifier, *wool.Log) {}
func (noopOutputSink) Info(string, ...any)                           {}
func (noopOutputSink) Error(string, ...any)                          {}
func (noopOutputSink) RegisterLoggingResource(string)                {}
