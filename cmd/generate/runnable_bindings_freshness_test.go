package generate

import (
	"context"
	"github.com/codefly-dev/cli/pkg/composition"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	runnablev0 "github.com/codefly-dev/core/generated/go/codefly/runnable/v0"
	"github.com/codefly-dev/core/resources"
	corerunnable "github.com/codefly-dev/core/runnable"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestCheckRunnableBindingsRefusesStaleValuesWithUnchangedKeys(t *testing.T) {
	ctx := context.Background()
	root, moduleDir := saveRunnableFixture(t, ctx, descriptorSet(t, ingestionFile(conformingOperation())), "0.1.0", connectEndpoint())
	t.Chdir(root)
	resetRunnablesFlags(t)
	resetRunnableBindingsFlags(t)
	require.NoError(t, RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}))
	publishFixtureConnectEndpoint(t, moduleDir, "connect", nil)
	require.NoError(t, RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil))
	ws, err := resources.LoadWorkspaceFromDir(ctx, root)
	require.NoError(t, err)
	env, err := environments.Select(ws, "local")
	require.NoError(t, err)
	target := filepath.Join(root, "configurations", "local", RunnableBindingsGroup+".env")
	raw, err := os.ReadFile(target)
	require.NoError(t, err)
	info := &basev0.ConfigurationInformation{Name: RunnableBindingsGroup}
	for _, line := range strings.Split(string(raw), "\n") {
		if key, value, found := strings.Cut(line, "="); found && !strings.HasPrefix(line, "#") {
			info.ConfigurationValues = append(info.ConfigurationValues, &basev0.ConfigurationValue{Key: key, Value: value})
		}
	}
	require.Len(t, info.ConfigurationValues, 1)
	require.NoError(t, CheckRunnableBindings(ctx, ws, env, info))
	for name, change := range map[string]func(*runnablev0.PreparedBinding){
		"address":  func(binding *runnablev0.PreparedBinding) { binding.Call.Address += "/stale" },
		"policy":   func(binding *runnablev0.PreparedBinding) { binding.Policy.MaxAttempts++ },
		"contract": func(binding *runnablev0.PreparedBinding) { binding.Contract.Input.Fields[0].Name += "Old" },
	} {
		t.Run(name, func(t *testing.T) {
			stale := proto.CloneOf(info)
			binding, err := corerunnable.DecodePrepared([]byte(stale.ConfigurationValues[0].Value))
			require.NoError(t, err)
			change(binding)
			binding.ContractDigest = ""
			value, err := corerunnable.EncodePrepared(binding)
			require.NoError(t, err)
			stale.ConfigurationValues[0].Value = string(value)
			require.ErrorContains(t, CheckRunnableBindings(ctx, ws, env, stale), "out of date")
		})
	}
	extra := proto.CloneOf(info)
	extra.ConfigurationValues = append(extra.ConfigurationValues, &basev0.ConfigurationValue{Key: "OBSOLETE", Value: "old"})
	require.ErrorContains(t, CheckRunnableBindings(ctx, ws, env, extra), "out of date")
	env.NamingScope = "another-run"
	require.ErrorContains(t, CheckRunnableBindings(ctx, ws, env, info), "out of date")
	// Checking never rewrites the consumer's file.
	after, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, raw, after)
}

func TestFreshCheckoutBindingCheckMaterializes(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	source := t.TempDir()
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = source
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "--quiet")
	git("config", "user.email", "review@example.invalid")
	git("config", "user.name", "Review Fixture")
	require.NoError(t, os.WriteFile(filepath.Join(source, resources.ModuleConfigurationName), []byte("name: sample\n"), 0600))
	git("add", ".")
	git("-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "fixture")
	git("-c", "tag.gpgSign=false", "tag", "v0.0.1")
	root := t.TempDir()
	manifest := "name: review\nlayout: modules\n" + composition.ModuleResolutionKey + ":\n  sample: git\nmodules:\n  - name: sample\n    source: file://" + source + "\n    version: v0.0.1\n"
	require.NoError(t, os.WriteFile(filepath.Join(root, resources.WorkspaceConfigurationName), []byte(manifest), 0600))
	t.Chdir(root)
	resetRunnableBindingsFlags(t)
	runnableBindingsCheck = true
	require.NoFileExists(t, filepath.Join(root, resources.LocalOverlayConfigurationName))
	require.NoError(t, RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil))
	require.FileExists(t, filepath.Join(root, resources.LocalOverlayConfigurationName))
	require.NoFileExists(t, filepath.Join(root, "configurations", "local", "runnable-bindings.env"))
	ws, err := resources.LoadWorkspaceFromDir(context.Background(), root)
	require.NoError(t, err)
	_, err = ws.LoadModules(context.Background())
	require.NoError(t, err)
}
