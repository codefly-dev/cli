package run

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/codefly-dev/cli/pkg/run/identity"
	"github.com/codefly-dev/core/wool"
	"github.com/codefly-dev/core/workloadidentity/native"
	"github.com/stretchr/testify/require"
)

type refreshLogCapture struct{ messages []string }

func (capture *refreshLogCapture) Process(log *wool.Log) {
	data, _ := json.Marshal(log)
	capture.messages = append(capture.messages, string(data))
}

func TestRefreshTokensLogsFailureAndRetriesOnNextPoll(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		vouch, err := identity.Start("example.test", "host", t.TempDir())
		require.NoError(t, err)
		workload := vouch.Workload("example", "scorer")
		vouch.Declare(workload)
		credential, err := vouch.Project(workload.Unique(), workload)
		require.NoError(t, err)
		require.NoError(t, os.Remove(credential.File))
		require.NoError(t, os.Mkdir(credential.File, 0o700))

		capture := &refreshLogCapture{}
		provider := wool.New(context.Background(), &wool.Resource{}).WithLogger(capture)
		defer provider.Done()
		ctx, cancel := context.WithCancel(provider.Inject(context.Background()))
		defer cancel()
		cluster := &launcher{vouch: vouch, stop: make(chan struct{})}
		done := make(chan struct{})
		go func() { defer close(done); cluster.refreshTokens(ctx) }()
		synctest.Wait()
		time.Sleep(native.RefreshInterval)
		synctest.Wait()
		logs := strings.Join(capture.messages, "\n")
		require.Contains(t, logs, "cannot refresh workload credentials")
		require.Contains(t, logs, workload.Unique())
		err = vouch.Refresh(time.Now())
		require.Error(t, err)
		var filesystemError *os.LinkError
		require.True(t, errors.As(err, &filesystemError))
		require.Contains(t, logs, filesystemError.Err.Error())

		// Repair the failed projection: waiting another 480 seconds would leave
		// an expired credential in place. The next short poll must retry it.
		require.NoError(t, os.Remove(credential.File))
		time.Sleep(5 * time.Second)
		synctest.Wait()
		info, err := os.Stat(credential.File)
		require.NoError(t, err)
		require.True(t, info.Mode().IsRegular())
		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("credential polling outlived the run context")
		}
	})
}
