package gitops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost/cell"
	"github.com/codefly-dev/core/solutionhost/modulecontract"
	"github.com/stretchr/testify/require"
)

// TestTheModuleContractKitRunsThroughTheRender drives core's module-contract
// kit through this repository's own entrypoint — the authority derivation,
// which reads the contract a module publishes — rather than through core's
// parser, which would prove nothing about this reader. Every fixture core
// refuses is refused here with the same sentinel and reason; every fixture
// core accepts is read, and whatever the derivation then refuses for its own
// reasons (a principal that is not the module's, a field the authority
// document cannot carry) is the renderer's verdict past the reader, not the
// reader's.
func TestTheModuleContractKitRunsThroughTheRender(t *testing.T) {
	ctx := context.Background()
	workspace, module := writeAuthorityWorkspace(t)
	// The kit's fixtures resolve their slots from the assistant group.
	require.NoError(t, os.WriteFile(filepath.Join(workspace.Dir(), "configurations", "staging", "assistant.env"),
		[]byte("MODEL_AUDIENCE=model-gateway\nMODEL_RESOURCE_KIND=modelservice.profiles\nMODEL_BINDING=model\nEVIDENCE_AUDIENCE=documents\nEVIDENCE_RESOURCE_KIND=documents.passages\nANNOTATIONS_PREFIX=annotations\n"), 0o600))
	workspace, err := resources.LoadWorkspaceFromDir(ctx, workspace.Dir())
	require.NoError(t, err)
	env := selectedEnvironment(t, workspace, "staging")
	services := loadServices(t, workspace, "shop", "api")
	units := []SolutionArtifactUnit{{Name: "api", Path: "services/api"}}
	modulecontract.Run(t, func(document []byte) error {
		require.NoError(t, os.WriteFile(filepath.Join(module.Dir(), modulecontract.FileName), document, 0o600))
		_, _, err := authorityInstancesOf(ctx, workspace, module, services, env, units)
		if err != nil && !errors.Is(err, modulecontract.ErrInvalid) && !errors.Is(err, modulecontract.ErrSchema) {
			// The reader accepted the contract; what the derivation refuses
			// after that is its own rule, not the kit's.
			return nil
		}
		return err
	})
}

// TestTheCellKitRunsThroughThePublisher drives core's cell kit through the one
// way this publisher reads a cell — the workspace's file and the delivered
// one alike — so a cell core refuses is refused here by the same name.
func TestTheCellKitRunsThroughThePublisher(t *testing.T) {
	cell.Run(t, func(document []byte) error {
		_, err := readCellFile(document)
		return err
	})
}
