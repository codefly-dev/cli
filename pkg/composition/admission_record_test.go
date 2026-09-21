package composition

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/codefly-dev/cli/pkg/internal/selectionguard"
	core "github.com/codefly-dev/core/composition"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
)

func recordedAdmissionFixture(t *testing.T) (*SelectionSession, *DeploymentFiles, core.DeploymentPolicy, *AdmissionInspection, time.Time) {
	t.Helper()
	session, files, options := stageFixtureWithSelection(t, []string{"left"}, []string{"builder"})
	staged, err := session.StageRender(t.Context(), files, options)
	require.NoError(t, err)
	data, err := os.ReadFile(staged.InputsFile)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, files))
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	now := time.Now().UTC()
	statement, err := json.Marshal(core.Qualification{
		Schema: "codefly/deployment-qualification/v1", Kind: "functional", Signer: "test-authority",
		SelectionIdentity: staged.Record.SelectionIdentity, RuntimeIdentity: staged.Record.RuntimeIdentity,
		ExecutionIdentity: staged.Record.ExecutionIdentity, BindingIdentity: staged.Record.BindingIdentity,
		ExpiresAt: now.Add(time.Hour),
	})
	require.NoError(t, err)
	files.Qualifications = []core.SignedQualification{{Statement: statement, Signature: ed25519.Sign(private, statement)}}
	policy := core.DeploymentPolicy{RequiredQualifications: []string{"functional"},
		QualificationSigners: map[string]map[string]ed25519.PublicKey{"functional": {"test-authority": public}}}
	destination := filepath.Join(options.OutputParent, "admission.json")
	admitted, err := session.RecordAdmission(t.Context(), files, policy, now, destination)
	require.NoError(t, err)
	data, err = os.ReadFile(destination)
	require.NoError(t, err)
	require.NotContains(t, string(data), files.Runtime[0].Path)
	require.NotContains(t, string(data), "not-for-output")
	var recorded AdmissionInspection
	require.NoError(t, json.Unmarshal(data, &recorded))
	require.Equal(t, admitted.Identity, recorded.Identity)
	info, err := os.Stat(destination)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	_, err = session.RecordAdmission(t.Context(), files, policy, now.Add(time.Second), destination)
	require.ErrorIs(t, err, os.ErrExist)
	unchanged, err := os.ReadFile(destination)
	require.NoError(t, err)
	require.Equal(t, data, unchanged)
	return session, files, policy, &recorded, now
}

func TestAdmissionRecordRechecksOriginalAndCurrentAdmission(t *testing.T) {
	session, files, policy, recorded, now := recordedAdmissionFixture(t)
	checked, err := session.RecheckAdmission(t.Context(), files, policy, recorded, recorded.Identity, now.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, recorded.Identity, checked.RecordedIdentity)
	require.NotEqual(t, recorded.Identity, checked.Current.Identity, "Core includes the admission instant in identity")
	require.Equal(t, recorded.Record.ExecutionIdentity, checked.Current.Record.ExecutionIdentity)
	require.ErrorContains(t, selectionguard.RejectUnboundExecution(session.Root), "deployment is blocked")

	_, err = session.RecheckAdmission(t.Context(), files, policy, recorded, "", now)
	require.ErrorContains(t, err, "retained admission identity")
	_, err = session.RecheckAdmission(t.Context(), files, policy, recorded, contentDigest([]byte("other approval")), now)
	require.ErrorContains(t, err, "retained admission identity")
	_, err = session.RecheckAdmission(t.Context(), files, policy, recorded, recorded.Identity, now.Add(-time.Second))
	require.ErrorContains(t, err, "future")
	_, err = session.RecheckAdmission(t.Context(), files, policy, recorded, recorded.Identity, now.Add(time.Hour))
	require.ErrorContains(t, err, "expired")
	policy.RequiredQualifications = append(policy.RequiredQualifications, "stateful")
	_, err = session.RecheckAdmission(t.Context(), files, policy, recorded, recorded.Identity, now)
	require.ErrorContains(t, err, "required stateful qualification")
	policy.RequiredQualifications = []string{"functional"}
	delete(policy.QualificationSigners["functional"], "test-authority")
	_, err = session.RecheckAdmission(t.Context(), files, policy, recorded, recorded.Identity, now)
	require.ErrorIs(t, err, core.ErrSignature)
	parent, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	destination := filepath.Join(parent, "refused.json")
	result, err := session.RecordAdmission(t.Context(), files, policy, now, destination)
	require.ErrorIs(t, err, core.ErrSignature)
	require.Nil(t, result)
	_, err = os.Stat(destination)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestAdmissionRecordRejectsTamperedRecordsAndEffectiveInputs(t *testing.T) {
	session, files, policy, recorded, now := recordedAdmissionFixture(t)
	recorded.Record.Bindings["extra"] = contentDigest([]byte("other target"))
	_, err := session.RecheckAdmission(t.Context(), files, policy, recorded, recorded.Identity, now)
	require.ErrorContains(t, err, "record content differs")
	delete(recorded.Record.Bindings, "extra")
	oldConfiguration := session.ConfigurationIdentity
	session.ConfigurationIdentity = contentDigest([]byte("new configuration"))
	_, err = session.RecheckAdmission(t.Context(), files, policy, recorded, recorded.Identity, now)
	require.Error(t, err)
	session.ConfigurationIdentity = oldConfiguration

	oldBinding := files.Bindings["target"]
	files.Bindings["target"] = contentDigest([]byte("different cluster"))
	_, err = session.RecheckAdmission(t.Context(), files, policy, recorded, recorded.Identity, now)
	require.Error(t, err)
	files.Bindings["target"] = oldBinding
	qualification := files.Qualifications[0]
	files.Qualifications = nil
	_, err = session.RecheckAdmission(t.Context(), files, policy, recorded, recorded.Identity, now)
	require.ErrorContains(t, err, "required functional qualification")
	files.Qualifications = []core.SignedQualification{qualification}

	var receipt basev0.ArtifactExecutionReceipt
	require.NoError(t, protojson.Unmarshal(files.Executions[0].Receipt, &receipt))
	path := filepath.Join(files.Executions[0].Directory, receipt.Outputs[0].Path)
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte("changed output"), 0o600))
	_, err = session.RecheckAdmission(t.Context(), files, policy, recorded, recorded.Identity, now)
	require.Error(t, err)
	oldReceipt := files.Executions[0].Receipt
	receipt.Outputs[0].Digest = contentDigest([]byte("changed output"))
	files.Executions[0].Receipt, err = protojson.Marshal(&receipt)
	require.NoError(t, err)
	_, err = session.RecheckAdmission(t.Context(), files, policy, recorded, recorded.Identity, now)
	require.ErrorContains(t, err, "different inputs")
	require.NoError(t, os.WriteFile(path, before, 0o600))
	files.Executions[0].Receipt = oldReceipt

	_, err = session.RecheckAdmission(t.Context(), files, policy, recorded, recorded.Identity, now)
	require.NoError(t, err, "restoring the exact evidence restores admission")
	require.NoError(t, os.WriteFile(files.Runtime[0].Path, []byte("private patch"), 0o600))
	_, err = session.RecheckAdmission(t.Context(), files, policy, recorded, recorded.Identity, now)
	require.ErrorIs(t, err, core.ErrDigestMismatch)
}

func TestAdmissionPublicationIsExclusiveAndCancellationPreservesRecords(t *testing.T) {
	parent, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	destination := filepath.Join(parent, "record.json")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, publishAdmissionRecord(ctx, destination, []byte("canceled")), context.Canceled)
	_, err = os.Stat(destination)
	require.ErrorIs(t, err, os.ErrNotExist)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, data := range []string{"first complete record", "second complete record"} {
		wg.Go(func() { results <- publishAdmissionRecord(t.Context(), destination, []byte(data)) })
	}
	wg.Wait()
	close(results)
	successes := 0
	for result := range results {
		if result == nil {
			successes++
		} else {
			require.ErrorIs(t, result, os.ErrExist)
		}
	}
	require.Equal(t, 1, successes)
	data, err := os.ReadFile(destination)
	require.NoError(t, err)
	require.Contains(t, []string{"first complete record", "second complete record"}, string(data))
	require.NoError(t, os.Symlink(destination, filepath.Join(parent, "alias.json")))
	require.ErrorIs(t, publishAdmissionRecord(t.Context(), filepath.Join(parent, "alias.json"), []byte("overwrite")), os.ErrExist)
	entries, err := os.ReadDir(parent)
	require.NoError(t, err)
	require.Len(t, entries, 2, "temporary records must be removed")
}
