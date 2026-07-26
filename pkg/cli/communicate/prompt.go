package communicate

import (
	"context"
	"os"

	"github.com/codefly-dev/cli/pkg/cli"
	agentscommunicate "github.com/codefly-dev/core/agents/communicate"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	"github.com/codefly-dev/core/wool"
	"golang.org/x/term"
)

// Prompt bridges agent questions to either an interactive terminal or a
// deterministic headless policy. Headless is an execution mode, not an error:
// CI, MCP, pipes, and automation must never attempt to open /dev/tty.
type Prompt struct {
	headless bool
}

func NewPrompt() *Prompt {
	headless := cli.WithDefault() ||
		!term.IsTerminal(int(os.Stdin.Fd())) ||
		!term.IsTerminal(int(os.Stdout.Fd()))
	return &Prompt{headless: headless}
}

// NewHeadlessPrompt returns a provider that accepts only protocol-declared
// defaults. It is exported for non-terminal callers that know their execution
// mode independently of process file descriptors.
func NewHeadlessPrompt() *Prompt {
	return &Prompt{headless: true}
}

func (h *Prompt) Answer(ctx context.Context, q *agentv0.Question) (*agentv0.Answer, error) {
	w := wool.Get(ctx).In("communicate.Answer")
	w.Trace("processing", wool.RequestField(q))
	if q == nil {
		return nil, w.NewError("cannot answer nil question")
	}
	if h.headless {
		return answerHeadless(ctx, q)
	}
	switch v := q.Value.(type) {
	case *agentv0.Question_Display:

		return Display(ctx, q.Message, v.Display)
	case *agentv0.Question_Confirm:
		return Confirm(ctx, q.Message, v.Confirm)
	case *agentv0.Question_Input:
		return Input(ctx, q.Message, v.Input)
	case *agentv0.Question_Choice:
		return Choice(ctx, q.Message, v.Choice)
	default:
		return nil, w.NewError("unknown question type: %v", q.Value)
	}
}

// answerHeadless resolves protocol-declared defaults without consulting a
// terminal. The policy lives in Core so CLI and embedded orchestration cannot
// drift. A plugin that omits a default still fails closed.
func answerHeadless(ctx context.Context, q *agentv0.Question) (*agentv0.Answer, error) {
	switch value := q.Value.(type) {
	case *agentv0.Question_Display:
		return Display(ctx, q.Message, value.Display)
	default:
		return agentscommunicate.AnswerDefault(q)
	}
}
