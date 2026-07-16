package orchestration

import (
	"context"
	"testing"
)

func TestSyncDriftPolicySynchronizesOnlyOrigin(t *testing.T) {
	policy, err := NewSyncDriftPolicy(context.Background(), nil, validationExecutor{}, "app/frontend")
	if err != nil {
		t.Fatal(err)
	}

	actions, err := policy.Execute(context.Background(), Action{Type: BuilderInit, Service: "infra/store"})
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 0 {
		t.Fatalf("dependency init scheduled sync: %v", actions)
	}

	actions, err = policy.Execute(context.Background(), Action{Type: BuilderInit, Service: "app/frontend"})
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].Type != BuilderSync || actions[0].Service != "app/frontend" {
		t.Fatalf("origin sync actions = %v", actions)
	}
}
