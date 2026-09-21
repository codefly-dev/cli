package composition

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/codefly-dev/cli/pkg/internal/selectionguard"
	"github.com/codefly-dev/core/policy"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func approvalFixture(t *testing.T) (*SelectionSession, *DeploymentFiles, *AdmissionInspection, *ApprovalAuthorityConfig, *ApprovalOptions, time.Time) {
	t.Helper()
	session, files, deploymentPolicy, recorded, now := recordedAdmissionFixture(t)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	config := &ApprovalAuthorityConfig{Audience: "test-product", Approver: policy.Principal{ID: "release-owner", Kind: policy.KindService, OrgID: "team"},
		Key: public, Policy: deploymentPolicy, Bindings: maps.Clone(files.Bindings)}
	installed, err := session.ConfigureApprovalAuthority(t.Context(), config, "")
	require.NoError(t, err)
	parent, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	return session, files, recorded, config, &ApprovalOptions{ExpectedIdentity: recorded.Identity, ExpectedAuthority: installed.Digest, SigningKey: private,
		ExpiresAt: now.Add(20 * time.Minute), Destination: filepath.Join(parent, "approval.json")}, now
}

func TestApprovalUsesHostAuthorityAndPreservesEffectGuards(t *testing.T) {
	session, files, recorded, config, options, now := approvalFixture(t)
	result, err := session.ApproveAdmission(t.Context(), files, recorded, options, now)
	require.NoError(t, err)
	require.Equal(t, recorded.Identity, result.AdmissionIdentity)
	data, err := os.ReadFile(options.Destination)
	require.NoError(t, err)
	var approval DeploymentApproval
	require.NoError(t, decodeSelectionJSON(data, &approval))
	verified, err := session.CheckApproval(t.Context(), files, &approval, now.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, result, verified)
	_, err = session.CheckApproval(t.Context(), files, &approval, now.Add(2*time.Minute))
	require.NoError(t, err, "inspection is not authorization consumption")
	require.ErrorContains(t, selectionguard.RejectUnboundExecution(session.Root), "deployment is blocked")
	_, err = session.ApproveAdmission(t.Context(), files, recorded, options, now)
	require.ErrorIs(t, err, os.ErrExist)
	after, err := os.ReadFile(options.Destination)
	require.NoError(t, err)
	require.Equal(t, data, after)
	_, err = session.CheckApproval(t.Context(), files, &approval, time.Unix(options.ExpiresAt.Unix(), 0))
	require.ErrorContains(t, err, "expired", "literal expiry overrides generic clock-skew allowance")

	// Installing even a weaker policy requires explicit host replacement, and
	// invalidates approval rather than retroactively approving under new rules.
	installed, err := session.InspectApprovalAuthority(t.Context())
	require.NoError(t, err)
	config.Policy.QualificationSigners["functional"]["additional-authority"] = config.Key
	_, err = session.ConfigureApprovalAuthority(t.Context(), config, installed.Digest)
	require.NoError(t, err)
	_, err = session.CheckApproval(t.Context(), files, &approval, now)
	require.ErrorContains(t, err, "host-owned authorization bindings")
}

func TestApprovalRejectsForeignOrMissingClaimsAndCurrentInputDrift(t *testing.T) {
	session, files, recorded, config, options, now := approvalFixture(t)
	authority, err := session.loadApprovalAuthority()
	require.NoError(t, err)
	_, wrongKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	for _, test := range []struct {
		name   string
		change func(*policy.MintInput)
		key    ed25519.PrivateKey
	}{
		{name: "wrong signer", key: wrongKey},
		{name: "wrong action", change: func(input *policy.MintInput) { input.Action = "workspace.mutation.apply" }},
		{name: "wrong audience", change: func(input *policy.MintInput) { input.AudienceID = "another-product" }},
		{name: "empty audience", change: func(input *policy.MintInput) { input.AudienceID = "" }},
		{name: "wrong admission", change: func(input *policy.MintInput) { input.Resource = contentDigest([]byte("other admission")) }},
		{name: "empty admission", change: func(input *policy.MintInput) { input.Resource = "" }},
		{name: "wrong policy", change: func(input *policy.MintInput) { input.CatalogDigest = contentDigest([]byte("weaker policy")) }},
		{name: "empty policy", change: func(input *policy.MintInput) { input.CatalogDigest = "" }},
		{name: "wrong target", change: func(input *policy.MintInput) { input.RequestDigest = contentDigest([]byte("other target")) }},
		{name: "empty target", change: func(input *policy.MintInput) { input.RequestDigest = "" }},
		{name: "wrong principal", change: func(input *policy.MintInput) { input.Principal.ID = "another-owner" }},
		{name: "wrong organization", change: func(input *policy.MintInput) { input.Principal.OrgID = "another-team" }},
		{name: "future", change: func(input *policy.MintInput) { input.NowFunc = func() time.Time { return now.Add(30 * time.Second) } }},
		{name: "before admission", change: func(input *policy.MintInput) { input.NowFunc = func() time.Time { return now.Add(-time.Second) } }},
		{name: "too many uses", change: func(input *policy.MintInput) { input.MaxUses = 2 }},
		{name: "beyond qualification", change: func(input *policy.MintInput) { input.TTL = 2 * time.Hour }},
		{name: "unknown caveat", change: func(input *policy.MintInput) { input.Caveats = map[string]any{"unrecognized": true} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			principal := config.Approver
			input := policy.MintInput{Principal: &principal, Action: approvalAction, Resource: recorded.Identity,
				AudienceID: config.Audience, CatalogDigest: authority.digest, RequestDigest: recorded.Record.BindingIdentity,
				MaxUses: 1, TTL: time.Minute, NowFunc: func() time.Time { return now }}
			if test.change != nil {
				test.change(&input)
			}
			key := test.key
			if key == nil {
				key = options.SigningKey
			}
			token, _, mintErr := policy.MintEd25519(input, key)
			require.NoError(t, mintErr)
			_, checkErr := session.CheckApproval(t.Context(), files, &DeploymentApproval{Admission: *recorded, Authorization: token}, now)
			require.Error(t, checkErr)
		})
	}
	_, err = session.ApproveAdmission(t.Context(), files, recorded, options, now)
	require.NoError(t, err)
	data, err := os.ReadFile(options.Destination)
	require.NoError(t, err)
	var approval DeploymentApproval
	require.NoError(t, json.Unmarshal(data, &approval))
	approval.Admission.Record.Bindings = nil
	_, err = session.CheckApproval(t.Context(), files, &approval, now)
	require.ErrorContains(t, err, "target bindings")
	approval.Admission.Record.Bindings = maps.Clone(config.Bindings)
	approval.Admission.Record.ApprovedAt = approval.Admission.Record.ApprovedAt.Add(-time.Minute)
	_, err = session.CheckApproval(t.Context(), files, &approval, now)
	require.Error(t, err, "signed approval cannot authenticate altered historical admission time")
	require.NoError(t, json.Unmarshal(data, &approval))
	require.NoError(t, os.WriteFile(files.Runtime[0].Path, []byte("private patch after approval"), 0o600))
	_, err = session.CheckApproval(t.Context(), files, &approval, now)
	require.Error(t, err)
}

func TestApprovalAuthorityReplacementIsExplicitScopedAndConcurrent(t *testing.T) {
	session, files, recorded, config, options, now := approvalFixture(t)
	installed, err := session.InspectApprovalAuthority(t.Context())
	require.NoError(t, err)
	_, err = session.ConfigureApprovalAuthority(t.Context(), config, "")
	require.ErrorContains(t, err, "explicit current digest")
	_, err = session.ConfigureApprovalAuthority(t.Context(), config, "wrong digest")
	require.ErrorContains(t, err, "explicit current digest")
	old, err := session.loadApprovalAuthority()
	require.NoError(t, err)
	config.Audience = "new-approved-audience"
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			_, configureErr := session.ConfigureApprovalAuthority(t.Context(), config, installed.Digest)
			results <- configureErr
		})
	}
	wg.Wait()
	close(results)
	successes := 0
	for configureErr := range results {
		if configureErr == nil {
			successes++
		} else {
			require.ErrorContains(t, configureErr, "explicit current digest")
		}
	}
	require.Equal(t, 1, successes)
	require.ErrorContains(t, old.unchanged(), "authority changed")
	_, err = session.ApproveAdmission(t.Context(), files, recorded, options, now)
	require.ErrorContains(t, err, "independently reviewed authority digest")
	current, err := session.InspectApprovalAuthority(t.Context())
	require.NoError(t, err)
	options.ExpectedAuthority = current.Digest
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = session.ApproveAdmission(ctx, files, recorded, options, now)
	require.ErrorIs(t, err, context.Canceled)
	_, err = os.Stat(options.Destination)
	require.ErrorIs(t, err, os.ErrNotExist)
	_, wrongKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	options.SigningKey = wrongKey
	_, err = session.ApproveAdmission(t.Context(), files, recorded, options, now)
	require.ErrorContains(t, err, "not the host-configured approval authority")
	require.NoError(t, os.Chmod(filepath.Dir(installed.Path), 0o777))
	_, err = session.InspectApprovalAuthority(t.Context())
	require.ErrorContains(t, err, "directory must not be group/world writable")
	require.NoError(t, os.Chmod(filepath.Dir(installed.Path), 0o700))
	require.NoError(t, os.Rename(installed.Path, installed.Path+".previous"))
	require.NoError(t, os.Symlink(installed.Path+".previous", installed.Path))
	_, err = session.InspectApprovalAuthority(t.Context())
	require.ErrorContains(t, err, "bounded regular file")
	require.NoError(t, os.Remove(installed.Path))
	require.NoError(t, os.Rename(installed.Path+".previous", installed.Path))
	t.Setenv(resources.CodeflyHomeEnv, session.Root)
	_, err = session.InspectApprovalAuthority(t.Context())
	require.ErrorContains(t, err, "outside the product")
}

func TestApprovalExpiresWhileWaitingForSelectionLock(t *testing.T) {
	session, files, recorded, _, options, now := approvalFixture(t)
	_, err := session.ApproveAdmission(t.Context(), files, recorded, options, now)
	require.NoError(t, err)
	data, err := os.ReadFile(options.Destination)
	require.NoError(t, err)
	var approval DeploymentApproval
	require.NoError(t, json.Unmarshal(data, &approval))
	holding, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- session.locked(t.Context(), func() error {
			close(holding)
			<-release
			return nil
		})
	}()
	<-holding
	checked, entered := make(chan error, 1), make(chan struct{})
	go func() {
		close(entered)
		_, checkErr := session.CheckApproval(t.Context(), files, &approval, time.Unix(options.ExpiresAt.Unix(), 0).Add(-100*time.Millisecond))
		checked <- checkErr
	}()
	<-entered
	time.Sleep(350 * time.Millisecond)
	close(release)
	require.NoError(t, <-done)
	require.ErrorContains(t, <-checked, "expired")
}

func TestApprovalAuthorityRequiresFullScopesAndNormalizesPolicy(t *testing.T) {
	_, _, _, valid, _, _ := approvalFixture(t)
	data, err := json.Marshal(valid)
	require.NoError(t, err)
	for _, change := range []func(*ApprovalAuthorityConfig){
		func(config *ApprovalAuthorityConfig) { config.Audience = "" },
		func(config *ApprovalAuthorityConfig) { config.Approver.ID = "" },
		func(config *ApprovalAuthorityConfig) { config.Key = nil },
		func(config *ApprovalAuthorityConfig) { config.Bindings = nil },
		func(config *ApprovalAuthorityConfig) { config.Bindings["target"] = "" },
		func(config *ApprovalAuthorityConfig) {
			config.Policy.RequiredQualifications = []string{"component-compatibility"}
		},
		func(config *ApprovalAuthorityConfig) { config.Policy.QualificationSigners = nil },
		func(config *ApprovalAuthorityConfig) {
			config.Policy.RequiredQualifications = []string{"functional", "functional"}
		},
		func(config *ApprovalAuthorityConfig) { config.Approver.Token = "credential-must-not-be-stored" },
	} {
		var config ApprovalAuthorityConfig
		require.NoError(t, json.Unmarshal(data, &config))
		change(&config)
		require.Error(t, validateApprovalAuthority(&config))
	}
	valid.Policy.RequiredQualifications = []string{"functional", "stateful"}
	valid.Policy.QualificationSigners["stateful"] = map[string]ed25519.PublicKey{"state-owner": valid.Key}
	first, err := normalizedApprovalAuthority(valid)
	require.NoError(t, err)
	valid.Policy.RequiredQualifications = []string{"stateful", "functional"}
	second, err := normalizedApprovalAuthority(valid)
	require.NoError(t, err)
	require.Equal(t, first, second)
	otherKey, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	valid.Policy.QualificationSigners["stateful"]["state-owner"] = otherKey
	changed, err := normalizedApprovalAuthority(valid)
	require.NoError(t, err)
	require.NotEqual(t, contentDigest(first), contentDigest(changed), "policy digest includes signer bytes, not just labels")
}

func TestApprovalAuthorityStorageCannotOccupyIndependentCheckout(t *testing.T) {
	session, _, _, config, _, _ := approvalFixture(t)
	home := t.TempDir()
	checkout := filepath.Join(home, "composition-authorities")
	require.NoError(t, os.Mkdir(checkout, 0o700))
	data, err := json.Marshal(map[string]string{"modules/left": checkout})
	require.NoError(t, err)
	localPath := filepath.Join(session.Root, LocalSelectionFile)
	require.NoError(t, os.MkdirAll(filepath.Dir(localPath), 0o700))
	require.NoError(t, os.WriteFile(localPath, data, 0o600))
	t.Setenv(resources.CodeflyHomeEnv, home)
	_, err = session.ConfigureApprovalAuthority(t.Context(), config, "")
	require.ErrorContains(t, err, "outside the product and local checkouts")
	entries, err := os.ReadDir(checkout)
	require.NoError(t, err)
	require.Empty(t, entries, "host authority setup must not write into independent checkout files")
}
