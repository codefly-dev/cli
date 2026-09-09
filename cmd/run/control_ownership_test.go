//go:build integration

package run

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/codefly-dev/core/network"
	"github.com/stretchr/testify/require"
)

// The audited collision is between separate processes racing for one derived
// control address, so these drive the real binary against real sockets rather
// than the in-process command functions.

var narratedInvocationID = regexp.MustCompile(`isolated invocation (inv[a-z2-7]{8})`)

// A run that loses the control address must say so before it owns anything.
// The failure a developer meets is two same-name checkouts (or two test
// packages) hashing to one port, so the message has to name the way out.
//
// "Owns nothing" is asserted through the identity the run narrates when it
// builds its flow: no identity means no flow, and therefore no agent, no
// container and no state directory. The same narration is asserted positively
// in the free-address case below and in the sibling test, so a reworded line
// cannot quietly turn this into an assertion about nothing.
func TestRunRefusesAControlAddressAnotherProcessOwns(t *testing.T) {
	// CLIServerPort reads this from whichever process calls it. Clearing it
	// here keeps the address this test claims identical to the one the child
	// derives; an inherited value would have them bind different ports and the
	// child would start a real run.
	t.Setenv("CODEFLY_CLI_SERVER_PORT", "")
	workspace := writeIsolationWorkspace(t, "cli-control-ownership")
	address := fmt.Sprintf("127.0.0.1:%d", network.CLIServerPort("cli-control-ownership"))

	squatter, err := net.Listen("tcp", address)
	require.NoError(t, err, "the derived control address must be bindable for this test to mean anything")

	output, err := runCodeflyService(t, workspace, "--cli-server")
	require.Error(t, err, "codefly ran on a control address it does not own:\n%s", output)
	require.Contains(t, output, "cannot own the codefly control server")
	require.Contains(t, output, "--naming-scope")
	require.NotRegexp(t, narratedInvocationID, output,
		"the losing run built its flow — and so claimed resources — before it owned its control address")

	// Positive control: the same command against a free address does reach
	// flow construction, so the assertion above is about ownership and not
	// about a narration that never happens.
	require.NoError(t, squatter.Close())
	output, _ = runCodeflyService(t, workspace, "--cli-server")
	require.Regexp(t, narratedInvocationID, output,
		"the run never reached flow construction even with its control address free")
}

// Two checkouts of one workspace, run at the same time with no isolation flag
// of any kind: the audited case. Each run must claim resource names the other
// cannot reach.
func TestConcurrentDisposableRunsClaimDistinctIdentities(t *testing.T) {
	first := writeIsolationWorkspace(t, "cli-invocation-identity")
	second := writeIsolationWorkspace(t, "cli-invocation-identity")

	ids := make([]string, 2)
	var wait sync.WaitGroup
	for i, workspace := range []string{first, second} {
		wait.Go(func() {
			id, err := awaitInvocationID(t, workspace)
			if err != nil {
				t.Errorf("run in %s: %v", workspace, err)
				return
			}
			ids[i] = id
		})
	}
	wait.Wait()

	require.NotEmpty(t, ids[0])
	require.NotEmpty(t, ids[1])
	require.NotEqual(t, ids[0], ids[1], "two concurrent disposable runs of one workspace share an identity")
}

// awaitInvocationID starts a real disposable run and reads the identity it
// narrates. The identity is fixed when the flow is built, before any agent is
// resolved, so the run is killed as soon as it reports one — this proves what
// the two runs own, not that an agent can be downloaded.
func awaitInvocationID(t *testing.T, workspace string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	command := codeflyCommand(ctx, t, workspace, "run", "service", "app/api", "--temporary-ports", "--headless")
	stdout, err := command.StdoutPipe()
	if err != nil {
		return "", err
	}
	command.Stderr = command.Stdout
	if err := command.Start(); err != nil {
		return "", err
	}
	defer func() {
		cancel()
		_ = command.Wait()
	}()

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		if found := narratedInvocationID.FindStringSubmatch(scanner.Text()); found != nil {
			return found[1], nil
		}
	}
	return "", fmt.Errorf("run never narrated an invocation identity")
}

func runCodeflyService(t *testing.T, workspace string, extra ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	args := append([]string{"run", "service", "app/api", "--temporary-ports", "--headless"}, extra...)
	output, err := codeflyCommand(ctx, t, workspace, args...).CombinedOutput()
	return string(output), err
}

func codeflyCommand(ctx context.Context, t *testing.T, workspace string, args ...string) *exec.Cmd {
	t.Helper()
	command := exec.CommandContext(ctx, codeflyBinary(t), args...)
	command.Dir = workspace
	// A derived CODEFLY_HOME keeps a developer's installed agents and daemon
	// state out of the run, and an inherited control port would override the
	// address these tests are about.
	command.Env = append(os.Environ(),
		"CODEFLY_HOME="+filepath.Join(workspace, ".home"),
		"CODEFLY_CLI_SERVER_PORT=",
	)
	command.WaitDelay = 10 * time.Second
	return command
}

// codeflyBinary resolves the CLI under test from PATH. The integration lane
// installs it from this checkout (.github/actions/install), so the tests cost
// a process launch rather than a link — building it here would put a full CLI
// link into every default test lane.
func codeflyBinary(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("codefly")
	require.NoError(t, err,
		"these tests drive the CLI under test as a subprocess: put it on PATH with `go install ./cmd/codefly`")
	return path
}

// writeIsolationWorkspace lays down a workspace whose origin service pins an
// agent that does not resolve. Every assertion here is about what the CLI
// claims before it reaches an agent, so a run that gets that far has already
// told us what we need.
func writeIsolationWorkspace(t *testing.T, name string) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"workspace.codefly.yaml": "name: " + name + "\nlayout: modules\nmodules:\n    - name: app\n",
		"modules/app/module.codefly.yaml": `kind: module
name: app
services:
    - name: api
`,
		"modules/app/services/api/service.codefly.yaml": `kind: service
name: api
version: 0.0.0
module: app
agent:
    kind: codefly:service
    name: go-grpc
    version: 0.0.16
    publisher: codefly.ai
`,
	}
	for rel, content := range files {
		path := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
	return root
}
