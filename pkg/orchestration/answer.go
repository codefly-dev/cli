package orchestration

import (
	"context"
	"fmt"

	agentscommunicate "github.com/codefly-dev/core/agents/communicate"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
)

// AnswerProvider answers interactive questions a builder plugin asks during
// Sync (e.g. confirm an overwrite). It is core's agents/communicate contract,
// reused here rather than duplicated, per doc.go's "no shadow DTO" rule.
type AnswerProvider = agentscommunicate.AnswerProvider

// headlessAnswerProvider accepts only protocol-declared defaults, matching
// pkg/cli/communicate's headless behavior without depending on pkg/cli. It is
// the Flow default; the cobra command injects an interactive terminal
// implementation at its call site to keep `codefly sync`'s existing behavior.
type headlessAnswerProvider struct{}

func (headlessAnswerProvider) Answer(_ context.Context, q *agentv0.Question) (*agentv0.Answer, error) {
	if q == nil {
		return nil, fmt.Errorf("cannot answer nil question")
	}
	name := "<unnamed>"
	if q.Message != nil && q.Message.Name != "" {
		name = q.Message.Name
	}
	switch v := q.Value.(type) {
	case *agentv0.Question_Display:
		return &agentv0.Answer{}, nil
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
