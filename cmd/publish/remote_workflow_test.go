package publish

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// Exercise the actual release step with a fake executable: a refused rehearsal
// must never proceed to publication, and success must use the same bump twice.
func TestRemoteWorkflowRehearsesBeforePublishing(t *testing.T) {
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Run string
				Env map[string]string
			}
		}
	}
	payload, err := os.ReadFile("../../.github/workflows/publish-agent.yml")
	require.NoError(t, err)
	require.NoError(t, yaml.Unmarshal(payload, &workflow))
	var script string
	for _, step := range workflow.Jobs["publish"].Steps {
		if step.Env["BUMP"] != "" && step.Env["GITHUB_TOKEN"] != "" {
			script = step.Run
		}
	}
	require.NotEmpty(t, script)
	for _, reject := range []bool{false, true} {
		t.Run(map[bool]string{false: "accepted", true: "refused"}[reject], func(t *testing.T) {
			dir := t.TempDir()
			log := filepath.Join(dir, "calls")
			fake := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$CALLS\"\n"
			if reject {
				fake += "exit 1\n"
			}
			require.NoError(t, os.WriteFile(filepath.Join(dir, "codefly"), []byte(fake), 0o700))
			command := exec.Command("bash", "-c", script)
			command.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "CALLS="+log, "BUMP=patch")
			err := command.Run()
			if reject {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			calls, err := os.ReadFile(log)
			require.NoError(t, err)
			want := "publish patch --ci --dry-run\n"
			if !reject {
				want += "publish patch --ci\n"
			}
			require.Equal(t, want, string(calls))
		})
	}
}

func TestCLIRemoteReleaseIsDispatchOnly(t *testing.T) {
	var caller struct {
		On   map[string]any
		Jobs map[string]struct {
			Uses    string
			Secrets map[string]string
		}
	}
	payload, err := os.ReadFile("../../.github/workflows/publish.yml")
	require.NoError(t, err)
	require.NoError(t, yaml.Unmarshal(payload, &caller))
	require.Len(t, caller.On, 1)
	require.Contains(t, caller.On, "workflow_dispatch")
	require.Len(t, caller.Jobs, 1)
	require.Equal(t, "./.github/workflows/publish-agent.yml", caller.Jobs["publish"].Uses)
	require.Equal(t, "${{ secrets.GH_PAT }}", caller.Jobs["publish"].Secrets["release-token"])
}

// The source bootstrap must invoke the owner's CLI build only in the CLI repo.
func TestSourcePublisherIsRestrictedToTheCLI(t *testing.T) {
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct{ Name, Run, If string }
		}
	}
	payload, err := os.ReadFile("../../.github/workflows/publish-agent.yml")
	require.NoError(t, err)
	require.NoError(t, yaml.Unmarshal(payload, &workflow))
	var script string
	for _, step := range workflow.Jobs["publish"].Steps {
		if step.Name == "Build this CLI's publisher from main" {
			require.Equal(t, "inputs.build-publisher", step.If)
			script = step.Run
		}
	}
	require.NotEmpty(t, script)
	for _, repository := range []string{"codefly-dev/cli", "other/repository"} {
		t.Run(repository, func(t *testing.T) {
			dir := t.TempDir()
			calls := filepath.Join(dir, "calls")
			fake := "#!/bin/sh\nprintf '%s\\n' \"$*\" > \"$CALLS\"\n"
			require.NoError(t, os.WriteFile(filepath.Join(dir, "go"), []byte(fake), 0o700))
			command := exec.Command("bash", "-c", script)
			command.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "CALLS="+calls, "REPOSITORY="+repository, "RUNNER_TEMP="+dir, "GITHUB_PATH="+filepath.Join(dir, "path"))
			err := command.Run()
			if repository == "codefly-dev/cli" {
				require.NoError(t, err)
				output, err := os.ReadFile(calls)
				require.NoError(t, err)
				require.Equal(t, "install ./cmd/codefly\n", string(output))
			} else {
				require.Error(t, err)
				_, err = os.Stat(calls)
				require.True(t, os.IsNotExist(err))
			}
		})
	}
}
