package orchestration_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/codefly-dev/cli/pkg/orchestration"
)

// blueRenderBuildSeconds are the 25 image build durations of one real
// `deploy gitops render --all --env blue`, in seconds, in the order that render
// built them. Their sum is the build phase's whole wall clock, because no two
// of those builds ever overlapped. That render went on to fail at its deploy
// step, on a missing secret declaration unrelated to building; every image was
// built first, which is why the phase is complete here.
//
// The last and largest of them, saas/frontend at 1020s, is the floor: no bound
// makes a build phase shorter than its longest single build.
var blueRenderBuildSeconds = []float64{
	24.5, 21.6, 18.6, 17.0, 20.8, 17.7, 12.4, 21.2, 20.2,
	332.5, 233.6, 28.4, 12.0, 183.1, 157.4, 134.7, 6.8,
	233.9, 406.9, 99.6, 152.6, 126.4, 29.3, 84.4, 1019.9,
}

// replayPolicy is a build phase of a known shape: one build action per service,
// each taking the time that service's image really took.
type replayPolicy struct {
	actions []orchestration.Action
	hold    map[string]time.Duration
	scale   float64
}

func (p *replayPolicy) GetExecutor(_ context.Context, action orchestration.Action) (orchestration.OutputProcessorFunc, error) {
	return func(_ context.Context) (*orchestration.OutputProperty, error) {
		time.Sleep(p.hold[action.Service])
		return orchestration.OnInit(), nil
	}, nil
}

func (p *replayPolicy) Execute(ctx context.Context, action orchestration.Action) ([]orchestration.Action, error) {
	executor, err := p.GetExecutor(ctx, action)
	if err != nil {
		return nil, err
	}
	if _, err = executor(ctx); err != nil {
		return nil, err
	}
	return nil, nil
}

func (p *replayPolicy) Restrict(context.Context, string) error { return nil }

func (p *replayPolicy) ConcurrentActions(actions []orchestration.Action) bool {
	for _, action := range actions {
		if action.Type != orchestration.BuilderBuild {
			return false
		}
	}
	return true
}

func newReplayPolicy(scale float64) *replayPolicy {
	policy := &replayPolicy{hold: map[string]time.Duration{}, scale: scale}
	for index, seconds := range blueRenderBuildSeconds {
		service := fmt.Sprintf("module%02d/service", index)
		policy.actions = append(policy.actions, orchestration.Action{Type: orchestration.BuilderBuild, Service: service})
		policy.hold[service] = time.Duration(seconds * scale * float64(time.Second))
	}
	return policy
}

// TestBlueRenderBuildPhaseMakespan replays a real render's build phase through
// the real playbook at several bounds and reports the wall clock of each.
//
// It is the measurement of the scheduling change and of nothing else: each
// build is a sleep of the duration that build really took, so the numbers show
// what the playbook does with a phase of that shape and do not include the CPU
// contention real concurrent docker builds add. Time is compressed by the scale
// factor so the whole sweep runs in seconds rather than in the hour the real
// durations would take.
//
// Gated because it only measures: CODEFLY_TEST_BUILD_MAKESPAN=1 to run it.
func TestBlueRenderBuildPhaseMakespan(t *testing.T) {
	if os.Getenv("CODEFLY_TEST_BUILD_MAKESPAN") == "" {
		t.Skip("set CODEFLY_TEST_BUILD_MAKESPAN=1 to measure the build phase makespan")
	}
	const scale = 0.01

	var serial time.Duration
	for _, bound := range []int{1, 2, 3, 4, 6, 8} {
		t.Run(fmt.Sprintf("at_once=%d", bound), func(t *testing.T) {
			t.Setenv("CODEFLY_BUILD_CONCURRENCY", fmt.Sprint(bound))
			ctx := context.Background()
			policy := newReplayPolicy(scale)

			playbook, err := orchestration.NewPlaybook(ctx, &orchestration.World{})
			require.NoError(t, err)
			playbook.WithPolicy(policy)
			last := policy.actions[len(policy.actions)-1]
			playbook.WithStoppingAfter(func(_ context.Context, action orchestration.Action) bool {
				return action.Service == last.Service && action.Type == last.Type
			})

			started := time.Now()
			require.NoError(t, playbook.Begin(ctx, policy.actions...))
			elapsed := time.Since(started)

			if bound == 1 {
				serial = elapsed
			}
			scaled := elapsed.Seconds() / scale
			line := fmt.Sprintf("at_once=%d wall=%.2fs (render scale: %.0fs = %.1f min)",
				bound, elapsed.Seconds(), scaled, scaled/60)
			if serial > 0 && bound > 1 {
				line += fmt.Sprintf("  speedup=%.2fx", serial.Seconds()/elapsed.Seconds())
			}
			t.Log(line)
		})
	}
}
