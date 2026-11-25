package communicate

import (
	"context"
	"strings"
	"testing"

	agentv1 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v1"
)

func TestHeadlessPromptUsesDeclaredDefaults(t *testing.T) {
	prompt := NewHeadlessPrompt()
	tests := []struct {
		name     string
		question *agentv1.Question
		assert   func(*testing.T, *agentv1.Answer)
	}{
		{
			name: "confirm",
			question: &agentv1.Question{Message: &agentv1.Message{Name: "hot-reload"}, Value: &agentv1.Question_Confirm{
				Confirm: &agentv1.Confirm{Default: true},
			}},
			assert: func(t *testing.T, answer *agentv1.Answer) {
				if answer.GetConfirm() == nil || !answer.GetConfirm().Confirmed {
					t.Fatalf("confirm answer = %#v, want declared true default", answer)
				}
			},
		},
		{
			name: "string input",
			question: &agentv1.Question{Message: &agentv1.Message{Name: "service-name"}, Value: &agentv1.Question_Input{
				Input: &agentv1.Input{Default: &agentv1.Input_StringDefault{StringDefault: "coordinator"}},
			}},
			assert: func(t *testing.T, answer *agentv1.Answer) {
				if got := answer.GetInput().GetStringValue(); got != "coordinator" {
					t.Fatalf("string answer = %q, want coordinator", got)
				}
			},
		},
		{
			name: "integer input",
			question: &agentv1.Question{Message: &agentv1.Message{Name: "replicas"}, Value: &agentv1.Question_Input{
				Input: &agentv1.Input{Default: &agentv1.Input_IntDefault{IntDefault: 3}},
			}},
			assert: func(t *testing.T, answer *agentv1.Answer) {
				if got := answer.GetInput().GetIntValue(); got != 3 {
					t.Fatalf("integer answer = %d, want 3", got)
				}
			},
		},
		{
			name: "choice",
			question: &agentv1.Question{Message: &agentv1.Message{Name: "runtime"}, Value: &agentv1.Question_Choice{
				Choice: &agentv1.Choice{
					Options:       []*agentv1.Message{{Name: "development"}, {Name: "production"}},
					DefaultOption: "production",
				},
			}},
			assert: func(t *testing.T, answer *agentv1.Answer) {
				if got := answer.GetChoice().GetOption(); got != "production" {
					t.Fatalf("choice answer = %q, want production", got)
				}
			},
		},
		{
			name: "explicitly empty selection",
			question: &agentv1.Question{Message: &agentv1.Message{Name: "features"}, Value: &agentv1.Question_Selection{
				Selection: &agentv1.Selection{
					Options: []*agentv1.Message{{Name: "grpc"}},
					Default: &agentv1.SelectionDefault{},
				},
			}},
			assert: func(t *testing.T, answer *agentv1.Answer) {
				if answer.GetSelection() == nil || len(answer.GetSelection().GetSelected()) != 0 {
					t.Fatalf("selection answer = %#v, want explicit empty selection", answer)
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
	questions := []*agentv1.Question{
		{Message: &agentv1.Message{Name: "region"}, Value: &agentv1.Question_Choice{Choice: &agentv1.Choice{Options: []*agentv1.Message{{Name: "east"}}}}},
		{Message: &agentv1.Message{Name: "features"}, Value: &agentv1.Question_Selection{Selection: &agentv1.Selection{Options: []*agentv1.Message{{Name: "grpc"}}}}},
		{Message: &agentv1.Message{Name: "token"}, Value: &agentv1.Question_Input{Input: &agentv1.Input{}}},
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
