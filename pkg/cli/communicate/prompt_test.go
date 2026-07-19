package communicate

import (
	"context"
	"strings"
	"testing"

	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
)

func TestHeadlessPromptUsesDeclaredDefaults(t *testing.T) {
	prompt := NewHeadlessPrompt()
	tests := []struct {
		name     string
		question *agentv0.Question
		assert   func(*testing.T, *agentv0.Answer)
	}{
		{
			name: "confirm",
			question: &agentv0.Question{Message: &agentv0.Message{Name: "hot-reload"}, Value: &agentv0.Question_Confirm{
				Confirm: &agentv0.Confirm{Default: true},
			}},
			assert: func(t *testing.T, answer *agentv0.Answer) {
				if answer.GetConfirm() == nil || !answer.GetConfirm().Confirmed {
					t.Fatalf("confirm answer = %#v, want declared true default", answer)
				}
			},
		},
		{
			name: "string input",
			question: &agentv0.Question{Message: &agentv0.Message{Name: "service-name"}, Value: &agentv0.Question_Input{
				Input: &agentv0.Input{Default: &agentv0.Input_StringDefault{StringDefault: "coordinator"}},
			}},
			assert: func(t *testing.T, answer *agentv0.Answer) {
				if got := answer.GetInput().GetStringValue(); got != "coordinator" {
					t.Fatalf("string answer = %q, want coordinator", got)
				}
			},
		},
		{
			name: "integer input",
			question: &agentv0.Question{Message: &agentv0.Message{Name: "replicas"}, Value: &agentv0.Question_Input{
				Input: &agentv0.Input{Default: &agentv0.Input_IntDefault{IntDefault: 3}},
			}},
			assert: func(t *testing.T, answer *agentv0.Answer) {
				if got := answer.GetInput().GetIntValue(); got != 3 {
					t.Fatalf("integer answer = %d, want 3", got)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			answer, err := prompt.Answer(context.Background(), test.question)
			if err != nil {
				t.Fatalf("Answer: %v", err)
			}
			test.assert(t, answer)
		})
	}
}

func TestHeadlessPromptRejectsUndeclaredDecisions(t *testing.T) {
	prompt := NewHeadlessPrompt()
	questions := []*agentv0.Question{
		{Message: &agentv0.Message{Name: "region"}, Value: &agentv0.Question_Choice{Choice: &agentv0.Choice{Options: []*agentv0.Message{{Name: "east"}}}}},
		{Message: &agentv0.Message{Name: "features"}, Value: &agentv0.Question_Selection{Selection: &agentv0.Selection{Options: []*agentv0.Message{{Name: "grpc"}}}}},
		{Message: &agentv0.Message{Name: "token"}, Value: &agentv0.Question_Input{Input: &agentv0.Input{}}},
	}
	for _, question := range questions {
		_, err := prompt.Answer(context.Background(), question)
		if err == nil {
			t.Fatalf("Answer(%s) succeeded without a declared default", question.Message.Name)
		}
		if !strings.Contains(err.Error(), "headless question") {
			t.Fatalf("Answer(%s) error = %q, want headless diagnostic", question.Message.Name, err)
		}
	}
}
