package manager_test

import (
	"context"
	"testing"

	"github.com/codefly-dev/cli/pkg/services/manager"
	"github.com/stretchr/testify/assert"
)

func TestSyncPolicyNoDependencies(t *testing.T) {
	ctx := context.Background()
	data := setup(t, manager.BuilderSync, execOnInit())
	// "Create"

	start := "billing/no_dependencies"

	err := data.policy.Restrict(ctx, start)
	assert.NoError(t, err)

	actions, err := data.policy.Execute(ctx, manager.Action{Type: manager.BuilderBegin, Service: start})
	assert.NoError(t, err)
	assert.Equal(t, createActions(start, manager.BuilderLoad), actions, "Expected no action to be triggered")

	actions, err = data.policy.Execute(ctx, manager.Action{Type: manager.BuilderLoad, Service: start})
	assert.NoError(t, err)
	assert.Equal(t, createActions(start, manager.BuilderInit), actions, "Expected no action to be triggered")

	actions, err = data.policy.Execute(ctx, manager.Action{Type: manager.BuilderInit, Service: start})
	assert.NoError(t, err)
	assert.Equal(t, createActions(start, manager.BuilderSync), actions, "Expected no action to be triggered")

}

func TestSyncPolicyOneDependency(t *testing.T) {
	ctx := context.Background()
	data := setup(t, manager.BuilderSync, execOnInit())
	// "Create"

	start := "billing/accounts"
	org := "management/organization"

	err := data.policy.Restrict(ctx, start)
	assert.NoError(t, err)

	actions, err := data.policy.Execute(ctx, manager.Action{Type: manager.BuilderBegin, Service: start})
	assert.NoError(t, err)
	assert.Equal(t, createCombinedActions([]string{org, start}, manager.BuilderLoad), actions, "Expected no action to be triggered")
}
