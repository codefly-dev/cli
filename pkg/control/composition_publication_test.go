package control

import (
	"bytes"
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/codefly-dev/cli/pkg/composition"
	"github.com/stretchr/testify/require"
)

func TestPreparedBuildPublicationClonesInputsConsumesAndClearsKeys(t *testing.T) {
	for _, ending := range []string{"apply", "expire", "rotate"} {
		t.Run(ending, func(t *testing.T) {
			plane := New().(*planeImpl)
			key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{5}, 32))
			request := &composition.BuildPublicationMutation{Workspace: t.TempDir(), Product: t.TempDir(),
				Inputs:  composition.DeploymentFiles{Bindings: map[string]string{"target": "retained"}},
				Options: composition.BuildPublicationOptions{ExpectedSelection: "inspected", Repository: "example.test/repo", Signers: map[string]composition.BuildSigningKey{"owner": {Signer: "builder", Key: key}}}}
			prepared, err := plane.PrepareMutation(t.Context(), Mutation{Kind: MutationCompositionBuildPublish, Payload: request})
			require.NoError(t, err)
			pending := plane.gate.pending[prepared.Token]
			retained := pending.mutation.Payload.(*composition.BuildPublicationMutation)
			request.Options.Repository = "attacker.test/repo"
			request.Inputs.Bindings["target"] = "changed"
			require.Equal(t, "example.test/repo", retained.Options.Repository)
			require.Equal(t, "retained", retained.Inputs.Bindings["target"])
			require.Equal(t, key, retained.Options.Signers["owner"].Key)
			switch ending {
			case "rotate":
				require.NoError(t, plane.ConfigureMutationAuthority(t.Context(), AuthorityConfig{Mode: AuthorityPrepared}))
			case "expire":
				pending.expiresAt = time.Now().Add(-time.Second)
				plane.gate.pending[prepared.Token] = pending
			}
			_, err = plane.ApplyPreparedMutation(t.Context(), prepared)
			require.Error(t, err, "missing workspace trust cannot publish")
			require.Equal(t, ed25519.PrivateKey(make([]byte, ed25519.PrivateKeySize)), retained.Options.Signers["owner"].Key)
			require.NotEqual(t, retained.Options.Signers["owner"].Key, key, "erasing prepared storage must not mutate caller key bytes")
			_, err = plane.ApplyPreparedMutation(t.Context(), prepared)
			require.ErrorContains(t, err, "already-consumed")
		})
	}
}
