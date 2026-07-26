package orchestration

import (
	"context"

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
// world is a live reference (not a copied OutputSink) so a later
// Flow.WithOutputSink still reaches Display narration answered before the
// override was installed.
type headlessAnswerProvider struct {
	world *World
}

func (h headlessAnswerProvider) Answer(_ context.Context, q *agentv0.Question) (*agentv0.Answer, error) {
	if q == nil {
		return agentscommunicate.AnswerDefault(q)
	}
	switch q.Value.(type) {
	case *agentv0.Question_Display:
		if h.world != nil && h.world.OutputSink != nil && q.Message != nil {
			h.world.OutputSink.Info("%s", q.Message.GetMessage())
		}
		return &agentv0.Answer{}, nil
	default:
		return agentscommunicate.AnswerDefault(q)
	}
}
