//go:build integration

package orchestration

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const teardownPostgresImage = "postgres:16-alpine"

func requireDocker(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatalf("this acceptance test needs docker on PATH: %v", err)
	}
	if output, err := exec.Command("docker", "info", "--format", "{{.ServerVersion}}").CombinedOutput(); err != nil {
		t.Fatalf("this acceptance test needs a reachable docker daemon: %v: %s", err, output)
	}
}

// startDisposablePostgres runs a throwaway Postgres and returns its container
// id. The container is removed when the test ends, whether or not the teardown
// under test stopped it.
func startDisposablePostgres(t *testing.T) string {
	t.Helper()
	requireDocker(t)

	output, err := exec.Command("docker", "run", "--detach", "--rm",
		"--env", "POSTGRES_PASSWORD=codefly",
		// initdb's fsyncs dominate container start and swing by 5x under load.
		// This database is created and destroyed inside one test, so durability
		// is pure cost; dropping it keeps the step's wall time predictable for
		// the CI gate that hosts it.
		"--env", "POSTGRES_INITDB_ARGS=--nosync",
		"--tmpfs", "/var/lib/postgresql/data:rw",
		teardownPostgresImage, "-c", "fsync=off").CombinedOutput()
	require.NoErrorf(t, err, "docker run %s: %s", teardownPostgresImage, output)
	container := strings.TrimSpace(string(output))
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "--force", container).Run()
	})

	waitForPostgres(t, container)
	_, err = psql(container, `CREATE TABLE drain (id serial primary key, committed_at timestamptz not null)`)
	require.NoError(t, err)
	return container
}

// initCompleteMarker is the line the official image's entrypoint prints after
// initdb, between shutting down the temporary server it ran the init scripts
// against and starting the real one.
const initCompleteMarker = "PostgreSQL init process complete"

// waitForPostgres blocks until the container's REAL server is accepting
// connections. Waiting on pg_isready alone is not enough: the image brings up a
// temporary server for initdb and then shuts it down, so pg_isready can answer
// a server that is about to close and the next statement fails against a
// restarting database. Gate on the entrypoint's init-complete marker first, so
// readiness refers to the server that outlives startup.
func waitForPostgres(t *testing.T, container string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	containerLogs := func() string {
		logs, _ := exec.Command("docker", "logs", container).CombinedOutput()
		return string(logs)
	}
	for !strings.Contains(containerLogs(), initCompleteMarker) {
		if time.Now().After(deadline) {
			t.Fatalf("disposable postgres never finished initdb (no %q in its logs): %s",
				initCompleteMarker, containerLogs())
		}
		time.Sleep(100 * time.Millisecond)
	}
	for {
		if err := exec.Command("docker", "exec", container, "pg_isready", "-U", "postgres").Run(); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("disposable postgres finished initdb but never accepted connections: %s", containerLogs())
		}
		time.Sleep(100 * time.Millisecond)
	}
	// One real statement: proves the server answering is the one that stays up.
	_, err := psql(container, `SELECT 1`)
	require.NoError(t, err, "postgres reported ready but rejected a statement")
}

// psql runs one statement in the container. It returns an error rather than
// failing the test because it is also called from an agent's Stop handler,
// which runs on a gRPC goroutine where t.FailNow is not allowed.
func psql(container, statement string) (string, error) {
	output, err := exec.Command("docker", "exec", container,
		"psql", "-U", "postgres", "-tAX", "-v", "ON_ERROR_STOP=1", "-c", statement).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("psql %q: %w: %s", statement, err, output)
	}
	return strings.TrimSpace(string(output)), nil
}

// TestFlowStopHoldsPostgresUntilItsConsumerCommits is the F09 acceptance case
// against real infrastructure: the api agent holds its drain open and commits
// its final transaction to a disposable Postgres while the flow is already
// stopping, and the agent that owns that Postgres must not begin stopping it
// until that commit has returned. Without the reverse-topological barrier the
// container stop races the write and the transaction is lost.
func TestFlowStopHoldsPostgresUntilItsConsumerCommits(t *testing.T) {
	container := startDisposablePostgres(t)

	var mu sync.Mutex
	var committedAt string
	api := &teardownAgent{drain: func(context.Context) error {
		// In-flight work the service still has to finish before it can answer.
		time.Sleep(500 * time.Millisecond)
		committed, err := psql(container, `INSERT INTO drain (committed_at) VALUES (now()) RETURNING committed_at`)
		if err != nil {
			return err
		}
		mu.Lock()
		committedAt = committed
		mu.Unlock()
		return nil
	}}

	database := &teardownAgent{drain: func(context.Context) error {
		output, err := exec.Command("docker", "stop", "--timeout", "0", container).CombinedOutput()
		if err != nil {
			return fmt.Errorf("docker stop %s: %w: %s", container, err, output)
		}
		return nil
	}}

	flow := teardownFlow(t,
		[]IManager{
			serveTeardownAgent(t, "app/database", database),
			serveTeardownAgent(t, "app/api", api),
		},
		teardownDependencies(t))

	require.NoError(t, flow.Stop())

	mu.Lock()
	committed := committedAt
	mu.Unlock()
	require.NotEmpty(t, committed, "the api never committed its final transaction")

	_, apiEnded := api.window()
	databaseBegan, _ := database.window()
	require.False(t, apiEnded.IsZero(), "the api drain never completed")
	require.True(t, databaseBegan.After(apiEnded),
		"postgres stop began at %s, before the api drain ended at %s", databaseBegan, apiEnded)

	require.Error(t, exec.Command("docker", "exec", container, "pg_isready", "-U", "postgres").Run(),
		"postgres is still running after teardown")

	receipt := flow.lastTeardownReceipt()
	require.Equal(t, 2, receipt.Layers)
	require.Equal(t, 0, entryFor(t, receipt, "app/api").Layer)
	require.Equal(t, 1, entryFor(t, receipt, "app/database").Layer)
	require.Equal(t, teardownStopped, entryFor(t, receipt, "app/database").Outcome)
	t.Logf("api drain ended %s, final commit at %s, postgres stop began %s", apiEnded, committed, databaseBegan)
}
