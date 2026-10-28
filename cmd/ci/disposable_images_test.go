package ci

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func TestDisposableCIRefusesPrerequisitesBeforeAnyScheduledAction(t *testing.T) {
	root, workspace := loadSchedulerFixture(t)
	path := filepath.Join(root, "modules/web/services/frontend/service.codefly.yaml")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte(strings.Replace(string(raw), "- name: gateway", "- name: organization\n      module: management\n      kind: build", 1)), 0600))
	plan := &Plan{Services: []PlannedService{{Service: "management/organization"}, {Service: "web/frontend"}}}
	for _, phase := range []string{"lint", "compile", "test", "build"} {
		t.Run(phase, func(t *testing.T) {
			calls := 0
			err := WithPlanOptions(t.Context(), workspace, plan, func(context.Context, *resources.Workspace, *resources.Module, *resources.Service) error {
				calls++
				return nil
			}, ScheduleOptions{Jobs: 2, Phase: phase, DisposableImages: true})
			require.ErrorContains(t, err, "disposable image input rebinding is unsupported")
			require.Zero(t, calls, "even the producer must not run before rejection")
		})
	}
}

func TestDisposableCISchedulesIndependentServiceWithoutUnrelatedRefusal(t *testing.T) {
	_, workspace := loadSchedulerFixture(t)
	plan := &Plan{Services: []PlannedService{{Service: "management/worker"}}}
	calls := 0
	err := WithPlanOptions(t.Context(), workspace, plan, func(context.Context, *resources.Workspace, *resources.Module, *resources.Service) error {
		calls++
		return nil
	}, ScheduleOptions{Phase: "build", DisposableImages: true})
	require.NoError(t, err)
	require.Equal(t, 1, calls)
}
