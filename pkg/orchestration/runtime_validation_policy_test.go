package orchestration

import (
	"context"
	"testing"
)

type validationExecutor struct{}

func (validationExecutor) GetExecutor(_ context.Context, _ Action) (OutputProcessorFunc, error) {
	return func(context.Context) (*OutputProperty, error) { return OnInit(), nil }, nil
}

func TestRuntimeValidationPolicyValidatesOnlyOrigin(t *testing.T) {
	policy, err := NewRuntimeValidationPolicy(context.Background(), nil, validationExecutor{}, "app/frontend", RuntimeLint)
	if err != nil {
		t.Fatal(err)
	}

	actions, err := policy.Execute(context.Background(), Action{Type: RuntimeInit, Service: "infra/store"})
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 0 {
		t.Fatalf("dependency init scheduled validation: %v", actions)
	}

	actions, err = policy.Execute(context.Background(), Action{Type: RuntimeInit, Service: "app/frontend"})
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].Type != RuntimeLint || actions[0].Service != "app/frontend" {
		t.Fatalf("origin validation actions = %v", actions)
	}
}

func TestRuntimeValidationPolicyRejectsNonValidationTerminal(t *testing.T) {
	if _, err := NewRuntimeValidationPolicy(context.Background(), nil, validationExecutor{}, "app/frontend", RuntimeTest); err == nil {
		t.Fatal("RuntimeTest was accepted as a static-validation terminal")
	}
}
