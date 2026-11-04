package orchestration

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// SeedBuildDurationForTest records a build duration the way a completed build
// records one, so the playbook tests in the external test package can give a
// phase a history to dispatch on.
//
// Exported for them and reachable from nowhere else: it is defined in a test
// file, so it is compiled into this package's test binary and does not exist in
// the CLI. It goes through the production writer rather than writing the file
// itself, so the on-disk format has exactly one definition and a test cannot
// pass against a format the CLI does not write.
func SeedBuildDurationForTest(t *testing.T, world *World, service string, took time.Duration) {
	t.Helper()
	require.NoError(t, world.buildDurations().record(context.Background(), service, took))
}

// WorldWithBuildHistoryForTest is a world whose build durations live in a
// temporary directory, for the same tests.
func WorldWithBuildHistoryForTest(t *testing.T, world *World) *World {
	t.Helper()
	workspace := &resources.Workspace{Name: "history"}
	workspace.WithDir(t.TempDir())
	world.Workspace = workspace
	return world
}

func workspaceWorld(t *testing.T) *World {
	t.Helper()
	return WorldWithBuildHistoryForTest(t, &World{})
}

func TestRecordedBuildDurationRoundTrips(t *testing.T) {
	world := workspaceWorld(t)
	durations := world.buildDurations()
	require.True(t, durations.enabled())

	require.NoError(t, durations.record(context.Background(), "saas/frontend", 1020*time.Second))

	seconds, ok := durations.seconds("saas/frontend")
	require.True(t, ok)
	require.InDelta(t, 1020.0, seconds, 0.001)

	// A service nothing was recorded for is absent, not zero: the ordering
	// treats absent as typical and zero as instant.
	_, ok = durations.seconds("saas/backend")
	require.False(t, ok)

	// The record nests under the service's own path, so two services can never
	// land on one file.
	_, err := os.Stat(filepath.Join(world.Workspace.Dir(), filepath.FromSlash(buildDurationsDir), "saas", "frontend.json"))
	require.NoError(t, err)
}

// A render re-times a service every time it really builds it, so the newest
// measurement is the one the next phase dispatches on.
func TestRecordingABuildDurationReplacesTheEarlierOne(t *testing.T) {
	durations := workspaceWorld(t).buildDurations()
	require.NoError(t, durations.record(context.Background(), "web/gateway", 300*time.Second))
	require.NoError(t, durations.record(context.Background(), "web/gateway", 42*time.Second))

	seconds, ok := durations.seconds("web/gateway")
	require.True(t, ok)
	require.InDelta(t, 42.0, seconds, 0.001)
}

// A record is written by an earlier run, possibly an older CLI. Everything that
// does not check out is absent rather than read as a duration.
func TestAnUnusableBuildDurationRecordIsAbsent(t *testing.T) {
	world := workspaceWorld(t)
	durations := world.buildDurations()
	root := filepath.Join(world.Workspace.Dir(), filepath.FromSlash(buildDurationsDir))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "module"), 0o755))

	for name, payload := range map[string]string{
		"garbage":       "not json",
		"other-schema":  `{"schema":99,"service":"module/garbage","seconds":12}`,
		"other-service": `{"schema":1,"service":"module/elsewhere","seconds":12}`,
		"no-duration":   `{"schema":1,"service":"module/no-duration","seconds":0}`,
	} {
		require.NoError(t, os.WriteFile(filepath.Join(root, "module", name+".json"), []byte(payload), 0o644))
		_, ok := durations.seconds("module/" + name)
		require.False(t, ok, "%s was read as a duration", name)
	}
}

// A flow with no workspace has nowhere to keep a record. It must dispatch in
// plan order, not panic on the way to docker.
func TestBuildDurationsToleratesNoWorkspace(t *testing.T) {
	var world *World
	require.False(t, world.buildDurations().enabled())
	require.NoError(t, world.buildDurations().record(context.Background(), "a/b", time.Second))
	_, ok := world.buildDurations().seconds("a/b")
	require.False(t, ok)
	require.Empty(t, world.buildDurations().known([]string{"a/b"}))

	empty := &World{Workspace: &resources.Workspace{Name: "nowhere"}}
	require.False(t, empty.buildDurations().enabled())
}

// Several builds of a phase finish at once, each recording its own duration.
// One file per service written by rename is what makes that safe; this is the
// test that watches it under -race.
func TestConcurrentRecordingDoesNotContend(t *testing.T) {
	durations := workspaceWorld(t).buildDurations()
	const services = 16
	var waiting sync.WaitGroup
	errs := make([]error, services)
	for index := range services {
		waiting.Add(1)
		go func() {
			defer waiting.Done()
			errs[index] = durations.record(context.Background(),
				serviceName(index), time.Duration(index+1)*time.Second)
		}()
	}
	waiting.Wait()
	for index := range services {
		require.NoError(t, errs[index])
		seconds, ok := durations.seconds(serviceName(index))
		require.True(t, ok)
		require.InDelta(t, float64(index+1), seconds, 0.001)
	}
}

func serviceName(index int) string {
	return "module/service" + string(rune('a'+index))
}

// The claim the recording site rests on, through the real build path with a
// docker stub: a build that ran is timed, and a build the reuse cache stood in
// for leaves the record alone.
//
// It matters because the two durations differ by orders of magnitude. If a
// configuration-only render — where every service is reused — re-timed every
// service, it would file the seconds of not building over the minutes of
// building, and the next real render would dispatch its longest build last
// again. This is the same reuse that TestImageIsKeptWhenNoImageInputChanged
// asserts, read through the duration record instead of the image.
func TestAReusedBuildDoesNotOverwriteTheRecordedDuration(t *testing.T) {
	harness := newImageCacheHarness(t, true, true)
	durations := &buildDurations{
		root: filepath.Join(harness.workspaceDir, filepath.FromSlash(buildDurationsDir)),
	}
	record, ok := durations.path("saas/api")
	require.True(t, ok)

	harness.build(t)
	require.Equal(t, 1, harness.docker.calls(t, "build"))
	timed, ok := durations.seconds("saas/api")
	require.True(t, ok, "a build that ran was not timed")
	require.Positive(t, timed)
	first, err := os.ReadFile(record)
	require.NoError(t, err)

	harness.write(t, filepath.Join(harness.workspaceDir, "configurations", "local.codefly.yaml"), "name: local\nvalue: two\n")
	harness.build(t)
	require.Equal(t, 1, harness.docker.calls(t, "build"), "the second build was not reused, so this proves nothing")

	again, err := os.ReadFile(record)
	require.NoError(t, err)
	require.Equal(t, string(first), string(again),
		"a reused build re-timed the service, which would file the cost of not building")
}

func buildActions(services ...string) []Action {
	actions := make([]Action, 0, len(services))
	for _, service := range services {
		actions = append(actions, Action{Type: BuilderBuild, Service: service})
	}
	return actions
}

// The cold-machine floor: a machine that has never timed a build dispatches the
// phase exactly as the plan handed it over, which is what it does today.
func TestWithNoHistoryTheDispatchOrderIsThePlanOrder(t *testing.T) {
	actions := buildActions("a/one", "b/two", "c/three", "d/four")
	require.Equal(t, []int{0, 1, 2, 3}, dispatchOrder(actions, nil))
	require.Equal(t, []int{0, 1, 2, 3}, dispatchOrder(actions, map[string]float64{}))
}

func TestTheLongestRecordedBuildIsDispatchedFirst(t *testing.T) {
	actions := buildActions("a/quick", "b/slow", "c/middling")
	order := dispatchOrder(actions, map[string]float64{
		"a/quick":    10,
		"b/slow":     1020,
		"c/middling": 300,
	})
	require.Equal(t, []int{1, 2, 0}, order)
}

// A service with no record is weighted at the median of the ones that have one,
// so it sorts into the middle of the phase rather than behind every fast build
// the machine happens to remember.
func TestAnUntimedBuildIsDispatchedAsATypicalOne(t *testing.T) {
	actions := buildActions("a/fast", "b/unknown", "c/slow", "d/mid")
	order := dispatchOrder(actions, map[string]float64{
		"a/fast": 5,
		"c/slow": 1020,
		"d/mid":  100,
	})
	// c/slow first, then the untimed build, weighted at the median of
	// {5, 100, 1020} and therefore ahead of a/fast and tied with d/mid, which
	// the plan's order settles. The untimed build is not sent to the back.
	require.Equal(t, []int{2, 1, 3, 0}, order)
}

// With exactly one build timed there is nothing to rank: the median of one
// record is that record, so every untimed build is assumed to cost the same and
// the plan's order stands. Stated here because it is the one state where
// history changes nothing — a render that failed partway through its first
// build phase on a new machine — and because the alternative, hoisting the one
// timed build on the strength of a single sample, is a guess about the other
// twenty-four.
func TestASingleRecordRanksNothing(t *testing.T) {
	actions := buildActions("a/one", "b/two", "c/timed")
	require.Equal(t, []int{0, 1, 2}, dispatchOrder(actions, map[string]float64{"c/timed": 1020}))
}

// Two services whose builds took the same time keep the order the plan gave
// them: the sort is stable, so one render's dispatch order is a function of the
// plan and the history and of nothing else.
func TestEqualDurationsKeepThePlanOrder(t *testing.T) {
	actions := buildActions("a/one", "b/two", "c/three")
	known := map[string]float64{"a/one": 100, "b/two": 100, "c/three": 100}
	for range 8 {
		require.Equal(t, []int{0, 1, 2}, dispatchOrder(actions, known))
	}
}
