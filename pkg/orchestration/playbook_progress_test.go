package orchestration_test

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/codefly-dev/cli/pkg/orchestration"
)

// reportingExecutor stands in for a snapshot's builders the way
// recordingExecutor does, and reports to the world's progress what each
// build would: one service's image is kept, every other one is built.
type reportingExecutor struct {
	world *orchestration.World
	kept  string
}

func (r *reportingExecutor) GetExecutor(_ context.Context, action orchestration.Action) (orchestration.OutputProcessorFunc, error) {
	return func(_ context.Context) (*orchestration.OutputProperty, error) {
		if action.Type != orchestration.BuilderBuild {
			return orchestration.OnInit(), nil
		}
		if action.Service == r.kept {
			orchestration.ReportKeptImageForTest(r.world)
			return orchestration.OnInit(), nil
		}
		time.Sleep(10 * time.Millisecond)
		orchestration.ReportBuildForTest(r.world, action.Service, 3*time.Second, "sha256:"+action.Service)
		return orchestration.OnInit(), nil
	}, nil
}

// runReportedSnapshot drives a whole snapshot rooted at root through the
// playbook with the world's build progress written to a buffer, and returns
// the lines it wrote and how many services the snapshot has.
func runReportedSnapshot(t *testing.T, root string) ([]string, int) {
	t.Helper()
	ctx := context.Background()
	data := setup(t, orchestration.BuilderSync, execOnInit())
	var out bytes.Buffer
	data.world.BuildProgress = orchestration.NewBuildProgress(&out)
	services := snapshotClosure(t, data.world.Dependencies, root)
	require.Greater(t, services, 1)

	executor := &reportingExecutor{world: data.world, kept: "billing/accounts"}
	policy, err := orchestration.NewSnapshotPolicy(ctx, data.world.Dependencies, executor)
	require.NoError(t, err)
	playbook, err := orchestration.NewPlaybook(ctx, data.world)
	require.NoError(t, err)
	playbook.WithPolicy(policy)
	deployed := 0
	playbook.WithStoppingAfter(func(_ context.Context, action orchestration.Action) bool {
		if action.Type == orchestration.BuilderDeploy {
			deployed++
		}
		return deployed == services
	})
	require.NoError(t, playbook.Begin(ctx, orchestration.Action{Type: orchestration.BuilderBegin, Service: root}))
	return strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n"), services
}

// The phase's total is every image the phase accounts for, built or kept, the
// kept count is said once when the phase is over, and a phase run one build
// at a time reports the same as one run several at a time.
func TestBuildPhaseReportsItsProgressToTheOperator(t *testing.T) {
	for _, bound := range []string{"4", "1"} {
		t.Run("at_once="+bound, func(t *testing.T) {
			t.Setenv("CODEFLY_BUILD_CONCURRENCY", bound)
			lines, services := runReportedSnapshot(t, concurrentRoot)

			built := services - 1
			require.Len(t, lines, 2*built+1, strings.Join(lines, "\n"))
			require.Equal(t, "1 image unchanged, kept", lines[len(lines)-1])
			for index := 1; index <= built; index++ {
				prefix := fmt.Sprintf("building %d/%d ", index, services)
				require.Equal(t, 1, countPrefixed(lines, prefix), "%s is not reported exactly once:\n%s", prefix, strings.Join(lines, "\n"))
				prefix = fmt.Sprintf("built %d/%d ", index, services)
				require.Equal(t, 1, countPrefixed(lines, prefix), "%s is not reported exactly once:\n%s", prefix, strings.Join(lines, "\n"))
			}
			require.Equal(t, 0, countPrefixed(lines, "building "+fmt.Sprint(services)+"/"), "a kept image took a build index")
			require.Equal(t, 1, countPrefixed(lines, "built 1/"+fmt.Sprint(services)+" "), "the built line lost its index")
			for _, line := range lines {
				if strings.HasPrefix(line, "built ") {
					require.Regexp(t, `^built \d+/\d+ [a-z]+/[a-z]+ 3s digest=sha256:[a-z]+/[a-z]+$`, line)
				}
			}
		})
	}
}

func countPrefixed(lines []string, prefix string) int {
	count := 0
	for _, line := range lines {
		if strings.HasPrefix(line, prefix) {
			count++
		}
	}
	return count
}
