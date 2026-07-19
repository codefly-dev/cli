package communicate

import (
	"context"
	"fmt"
	"os"

	"github.com/codefly-dev/cli/pkg/cli"
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

// answerHeadless resolves defaults without consulting a terminal. Confirm and
// typed Input carry defaults in the wire contract. Choice and Selection do
// not, so silently picking an option would turn CI into an architectural
// decision-maker; those question types fail with a precise error instead.
func answerHeadless(ctx context.Context, q *agentv0.Question) (*agentv0.Answer, error) {
	name := "<unnamed>"
	if q.Message != nil && q.Message.Name != "" {
		name = q.Message.Name
	}
	switch v := q.Value.(type) {
	case *agentv0.Question_Display:
		return Display(ctx, q.Message, v.Display)
	case *agentv0.Question_Confirm:
		return &agentv0.Answer{Value: &agentv0.Answer_Confirm{Confirm: &agentv0.ConfirmAnswer{Confirmed: v.Confirm.GetDefault()}}}, nil
	case *agentv0.Question_Input:
		if v.Input == nil || v.Input.Default == nil {
			return nil, fmt.Errorf("headless question %q requires input but declares no default", name)
		}
		switch value := v.Input.Default.(type) {
		case *agentv0.Input_StringDefault:
			return &agentv0.Answer{Value: &agentv0.Answer_Input{Input: &agentv0.InputAnswer{Answer: &agentv0.InputAnswer_StringValue{StringValue: value.StringDefault}}}}, nil
		case *agentv0.Input_IntDefault:
			return &agentv0.Answer{Value: &agentv0.Answer_Input{Input: &agentv0.InputAnswer{Answer: &agentv0.InputAnswer_IntValue{IntValue: value.IntDefault}}}}, nil
		default:
			return nil, fmt.Errorf("headless question %q has an unsupported input default %T", name, value)
		}
	case *agentv0.Question_Choice:
		return nil, fmt.Errorf("headless question %q requires a choice but the protocol declares no default option", name)
	case *agentv0.Question_Selection:
		return nil, fmt.Errorf("headless question %q requires a selection but the protocol declares no default selection", name)
	default:
		return nil, fmt.Errorf("headless question %q has unknown type %T", name, q.Value)
	}
}
