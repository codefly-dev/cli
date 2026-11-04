package orchestration

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ReportKeptImageForTest reports to world's progress what a build whose image
// was up to date reports, for a test standing an executor in for the builders.
func ReportKeptImageForTest(world *World) {
	world.buildProgress().keptImage()
}

// ReportBuildForTest reports to world's progress what one recipe build reports
// from start to finish, for the same tests.
func ReportBuildForTest(world *World, service string, took time.Duration, digest string) {
	progress := world.buildProgress()
	ordinal := progress.building(service, "app")
	progress.built(ordinal, service, took, digest)
}

// The lines are what an operator reads a 25-minute phase by, so they are
// asserted exactly: an index is assigned when a build starts and kept by its
// built line however the builds finish, and the images kept are said once,
// when the phase ends, with a total the indices and the kept count add up to.
func TestBuildProgressReportsEachBuildAndTheKeptCountOnce(t *testing.T) {
	var out bytes.Buffer
	progress := NewBuildProgress(&out)

	endPhase := progress.beginPhase(5)
	progress.keptImage()
	accounts := progress.building("saas/accounts", "app")
	progress.keptImage()
	gateway := progress.building("saas/gateway", "app")
	worker := progress.building("saas/worker", "job")
	// Later builds finish first.
	progress.built(worker, "saas/worker", 12*time.Second, "")
	progress.built(gateway, "saas/gateway", 42*time.Second+300*time.Millisecond, "sha256:abc")
	progress.built(accounts, "saas/accounts", 2*time.Minute+5*time.Second, "sha256:def")
	endPhase()

	require.Equal(t, strings.Join([]string{
		"building 1/5 saas/accounts recipe=app",
		"building 2/5 saas/gateway recipe=app",
		"building 3/5 saas/worker recipe=job",
		"built 3/5 saas/worker 12s",
		"built 2/5 saas/gateway 42s digest=sha256:abc",
		"built 1/5 saas/accounts 2m5s digest=sha256:def",
		"2 images unchanged, kept",
	}, "\n")+"\n", out.String())
}

func TestBuildProgressSaysNothingOfAPhaseThatKeptNothing(t *testing.T) {
	var out bytes.Buffer
	progress := NewBuildProgress(&out)
	progress.beginPhase(1)()
	require.Empty(t, out.String())

	endPhase := progress.beginPhase(1)
	progress.keptImage()
	endPhase()
	require.Equal(t, "1 image unchanged, kept\n", out.String())
}

func TestBuildProgressReportsPushes(t *testing.T) {
	var out bytes.Buffer
	progress := NewBuildProgress(&out)
	endPhase := progress.beginPhase(2)
	first := progress.pushing("saas/accounts", "registry/accounts:1.2.3")
	progress.pushed(first, "saas/accounts", "registry/accounts:1.2.3", 12*time.Second)
	endPhase()
	require.Equal(t, "pushing 1/2 saas/accounts registry/accounts:1.2.3\npushed 1/2 saas/accounts registry/accounts:1.2.3 12s\n", out.String())
}

// A phase's builds report from their own goroutines; every line must still
// arrive whole, with every index used exactly once.
func TestBuildProgressLinesDoNotInterleaveAcrossBuilds(t *testing.T) {
	var out bytes.Buffer
	progress := NewBuildProgress(&out)
	const builds = 32
	endPhase := progress.beginPhase(builds)
	var group sync.WaitGroup
	for range builds {
		group.Go(func() {
			ordinal := progress.building("saas/service", "app")
			progress.built(ordinal, "saas/service", time.Second, "")
		})
	}
	group.Wait()
	endPhase()

	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	require.Len(t, lines, 2*builds)
	seen := map[string]int{}
	for _, line := range lines {
		fields := strings.Fields(line)
		require.Len(t, fields, 4, line)
		require.Contains(t, []string{"building", "built"}, fields[0], line)
		seen[fields[0]+" "+fields[1]]++
	}
	require.Len(t, seen, 2*builds, "an index was reused or skipped: %v", seen)
}

// Nothing watches most flows, and they must cost nothing and print nothing.
func TestNoBuildProgressIsSilent(t *testing.T) {
	var progress *BuildProgress
	require.Nil(t, NewBuildProgress(nil))
	endPhase := progress.beginPhase(3)
	progress.keptImage()
	progress.built(progress.building("saas/accounts", "app"), "saas/accounts", time.Second, "")
	progress.pushed(progress.pushing("saas/accounts", "image"), "saas/accounts", "image", time.Second)
	endPhase()
	var world *World
	require.Nil(t, world.buildProgress())
}
