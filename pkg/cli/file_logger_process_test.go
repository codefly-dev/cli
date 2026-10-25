package cli

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/wool"
	"github.com/stretchr/testify/require"
)

func TestInvocationLogProcess(t *testing.T) {
	if os.Getenv("CODEFLY_LOG_PROCESS_TEST") != "1" {
		return
	}
	EnableFileLogging()
	fl := GetFileLogger()
	require.NotNil(t, fl)
	defer fl.Close()
	fl.Process(&wool.Log{Level: wool.INFO, Message: os.Getenv("CODEFLY_LOG_PROCESS_NAME") + " preparation"})
	fmt.Println(fl.Path())
	fl.maxSize = 1 // Force every later record into a new segment.
	input := bufio.NewScanner(os.Stdin)
	for input.Scan() {
		fl.Process(&wool.Log{Level: wool.INFO, Message: input.Text()})
	}
	require.NoError(t, input.Err())
}

func TestSeparateProcessesKeepLateShutdownInTheOldInvocation(t *testing.T) {
	root := t.TempDir()
	type child struct {
		cmd    *exec.Cmd
		stdin  io.WriteCloser
		path   string
		stderr bytes.Buffer
		waited bool
	}
	start := func(name string) *child {
		c := &child{cmd: exec.Command(os.Args[0], "-test.run=^TestInvocationLogProcess$")}
		c.cmd.Env = append(os.Environ(), "CODEFLY_HOME="+root, "CODEFLY_LOG_PROCESS_TEST=1", "CODEFLY_LOG_PROCESS_NAME="+name)
		var err error
		c.stdin, err = c.cmd.StdinPipe()
		require.NoError(t, err)
		stdout, err := c.cmd.StdoutPipe()
		require.NoError(t, err)
		c.cmd.Stderr = &c.stderr
		require.NoError(t, c.cmd.Start())
		t.Cleanup(func() {
			_ = c.stdin.Close()
			if !c.waited {
				_ = c.cmd.Process.Kill()
				_ = c.cmd.Wait()
			}
		})
		scanner := bufio.NewScanner(stdout)
		require.True(t, scanner.Scan(), "child must report its allocated log")
		c.path = scanner.Text()
		return c
	}
	old := start("old")
	fresh := start("fresh")
	require.NotEqual(t, filepath.Dir(old.path), filepath.Dir(fresh.path))
	_, err := fmt.Fprintln(fresh.stdin, "fresh running")
	require.NoError(t, err)
	_, err = fmt.Fprintln(old.stdin, "old late shutdown\nold final shutdown")
	require.NoError(t, err)
	for _, c := range []*child{old, fresh} {
		require.NoError(t, c.stdin.Close())
		err := c.cmd.Wait()
		c.waited = true
		require.NoError(t, err, c.stderr.String())
	}
	contents := func(path string) string {
		files, err := filepath.Glob(filepath.Join(filepath.Dir(path), "*.log"))
		require.NoError(t, err)
		var data strings.Builder
		for _, file := range files {
			b, err := os.ReadFile(file)
			require.NoError(t, err)
			data.Write(b)
		}
		return data.String()
	}
	oldLog, freshLog := contents(old.path), contents(fresh.path)
	require.Contains(t, oldLog, "old preparation")
	require.Contains(t, oldLog, "old late shutdown")
	require.Contains(t, oldLog, "old final shutdown")
	require.NotContains(t, oldLog, "fresh ")
	require.Contains(t, freshLog, "fresh preparation")
	require.Contains(t, freshLog, "fresh running")
	require.NotContains(t, freshLog, "old ")
}
