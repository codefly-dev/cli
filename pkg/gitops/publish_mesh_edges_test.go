package gitops

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// The mesh edges a render records are the composition's reach as the cell's
// applier compiles it, and the applier reads the PUBLISHED inventory: a
// publication rebuilds that inventory from the render's, and must carry the
// edges across unchanged, or the cell admits nothing.
func TestPublishCarriesTheRendersMeshEdges(t *testing.T) {
	ctx, workspace, remote := developerPublishWorkspace(t)
	path := filepath.Join(workspace.Dir(), "deployments", "modules", "payments", InventoryFilename)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var rendered Inventory
	require.NoError(t, json.Unmarshal(data, &rendered))
	rendered.MeshEdges = &MeshEdges{
		SchemaVersion: MeshEdgesSchemaVersion, TrustDomain: "cluster.example", Gateway: "payments/api",
		Edges: []MeshEdge{{
			Module: "payments", Service: "api", Namespace: "payments", Account: "api",
			Principal: "spiffe://cluster.example/ns/payments/sa/api",
			Endpoint:  "rest", Port: 8080, TargetPort: 8080, Ingress: true,
		}},
	}
	require.NoError(t, writeCanonicalInventory(path, &rendered))

	request := PublishRequest{
		Module: "payments", Environment: "production", Local: true, Signer: &fakeSigner{nonce: true},
		PromotionBranch: "codefly/promote-payments-production",
	}
	plan, err := PlanPublish(ctx, workspace, &request)
	require.NoError(t, err)
	result, err := Publish(ctx, workspace, &PublishMutation{Request: request, PlanID: plan.ID, Carriers: plan.Carriers}, preparedPermit)
	require.NoError(t, err)

	var published Inventory
	require.NoError(t, json.Unmarshal([]byte(gitOutput(t, "", "--git-dir", remote, "show", result.Commit+":"+result.Path+"/"+InventoryFilename)), &published))
	require.Equal(t, rendered.MeshEdges, published.MeshEdges, "the publication carries the render's mesh edges unchanged")
}
