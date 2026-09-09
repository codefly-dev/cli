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

	"github.com/codefly-dev/core/architecture"
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
		teardownPostgresImage).CombinedOutput()
	require.NoErrorf(t, err, "docker run %s: %s", teardownPostgresImage, output)
	container := strings.TrimSpace(string(output))
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "--force", container).Run()
	})

	deadline := time.Now().Add(90 * time.Second)
	for {
		if err := exec.Command("docker", "exec", container, "pg_isready", "-U", "postgres").Run(); err == nil {
			break
		}
		if time.Now().After(deadline) {
			logs, _ := exec.Command("docker", "logs", container).CombinedOutput()
			t.Fatalf("disposable postgres never became ready: %s", logs)
		}
		time.Sleep(500 * time.Millisecond)
	}

	_, err = psql(container, `CREATE TABLE drain (id serial primary key, committed_at timestamptz not null)`)
	require.NoError(t, err)
	return container
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
		[]architecture.ServiceDependency{edge("app/database", "app/api")})

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

	receipt := flow.LastTeardown()
	require.Equal(t, 2, receipt.Layers)
	require.Equal(t, 0, entryFor(t, receipt, "app/api").Layer)
	require.Equal(t, 1, entryFor(t, receipt, "app/database").Layer)
	require.Equal(t, TeardownStopped, entryFor(t, receipt, "app/database").Outcome)
	t.Logf("api drain ended %s, final commit at %s, postgres stop began %s", apiEnded, committed, databaseBegan)
}
