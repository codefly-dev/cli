package composition

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/cli/pkg/internal/selectionguard"
	"github.com/codefly-dev/core/policy"
	"github.com/stretchr/testify/require"
)

func approvalUseFixture(t *testing.T) (*SelectionSession, *DeploymentFiles, *DeploymentApproval, *ApprovalInspection, time.Time) {
	t.Helper()
	session, files, recorded, _, options, now := approvalFixture(t)
	inspected, err := session.ApproveAdmission(t.Context(), files, recorded, options, now)
	require.NoError(t, err)
	data, err := os.ReadFile(options.Destination)
	require.NoError(t, err)
	var approval DeploymentApproval
	require.NoError(t, json.Unmarshal(data, &approval))
	// Consumption happens after minting. Reusing the pre-admission fixture
	// instant can make a newly minted token appear future-issued under load.
	return session, files, &approval, inspected, time.Now()
}

func TestApprovalUseIsDurableWithoutImplyingDeployment(t *testing.T) {
	session, files, approval, checked, now := approvalUseFixture(t)
	used, err := session.ReserveApproval(t.Context(), files, approval, checked.AuthorityDigest, now)
	require.NoError(t, err)
	require.Equal(t, *checked, used.Approval)
	require.Equal(t, approval.Admission.Record.ExecutionIdentity, used.Admission.Record.ExecutionIdentity)
	require.Equal(t, approval.Admission.Record.Bindings, used.Admission.Record.Bindings)
	require.ErrorIs(t, selectionguard.RejectUnboundExecution(session.Root), selectionguard.ErrUnboundExecution)
	// Reopening the host/session does not create a fresh replay tracker.
	reopened, err := NewSelectionSession(filepath.Dir(session.trustPath), session.Root, session.ConfigurationIdentity)
	require.NoError(t, err)
	_, err = reopened.ReserveApproval(t.Context(), files, approval, checked.AuthorityDigest, now)
	require.ErrorIs(t, err, ErrApprovalAlreadyUsed)
	inspected, err := reopened.InspectApprovalUse(t.Context(), checked.UseIdentity)
	require.NoError(t, err)
	require.Equal(t, used, inspected)
	_, err = reopened.CheckApproval(t.Context(), files, approval, now)
	require.NoError(t, err, "evidence inspection does not turn into consumption or effect authorization")
	path, _, err := session.approvalUsePath(checked.UseIdentity)
	require.NoError(t, err)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NotContains(t, string(data), approval.Authorization)
	require.NotContains(t, string(data), files.Runtime[0].Path)
	require.NotContains(t, string(data), "not-for-output")
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	_, err = reopened.ReserveApproval(t.Context(), files, approval, checked.AuthorityDigest, checked.ExpiresAt)
	require.ErrorContains(t, err, "expired")
	inspected, err = reopened.InspectApprovalUse(t.Context(), checked.UseIdentity)
	require.NoError(t, err, "historical use survives approval expiry")
	require.Equal(t, used, inspected)
}

func TestApprovalUseRefusalsDoNotConsumeAndCurrentInputsAreRequired(t *testing.T) {
	session, files, approval, checked, now := approvalUseFixture(t)
	for _, expected := range []string{"", contentDigest([]byte("foreign authority"))} {
		_, err := session.ReserveApproval(t.Context(), files, approval, expected, now)
		require.Error(t, err)
	}
	_, err := session.ReserveApproval(t.Context(), files, approval, checked.AuthorityDigest, checked.ExpiresAt)
	require.ErrorContains(t, err, "expired")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = session.ReserveApproval(ctx, files, approval, checked.AuthorityDigest, now)
	require.ErrorIs(t, err, context.Canceled)
	data, err := os.ReadFile(files.Runtime[0].Path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(files.Runtime[0].Path, []byte("drift"), 0o600))
	_, err = session.ReserveApproval(t.Context(), files, approval, checked.AuthorityDigest, now)
	require.Error(t, err)
	require.NoError(t, os.WriteFile(files.Runtime[0].Path, data, 0o600))
	_, err = session.InspectApprovalUse(t.Context(), checked.UseIdentity)
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = session.ReserveApproval(t.Context(), files, approval, checked.AuthorityDigest, now)
	require.NoError(t, err, "none of the rejected calls spent the approval")
}

func TestApprovalUseIdentitySurvivesReencodingAndPolicyRotation(t *testing.T) {
	session, files, recorded, config, options, now := approvalFixture(t)
	authority, err := session.loadApprovalAuthority()
	require.NoError(t, err)
	mint := func(digest string, ttl time.Duration) *DeploymentApproval {
		t.Helper()
		token, _, mintErr := policy.MintEd25519(policy.MintInput{Principal: &config.Approver, Action: approvalAction,
			Resource: recorded.Identity, AudienceID: config.Audience, CatalogDigest: digest, RequestDigest: recorded.Record.BindingIdentity,
			TTL: ttl, MaxUses: 1, NowFunc: func() time.Time { return now }, IDFunc: func() string { return "same-authenticated-token-id" }}, options.SigningKey)
		require.NoError(t, mintErr)
		return &DeploymentApproval{Admission: *recorded, Authorization: token}
	}
	first := mint(authority.digest, time.Minute)
	used, err := session.ReserveApproval(t.Context(), files, first, authority.digest, now)
	require.NoError(t, err)
	reissued := mint(authority.digest, 2*time.Minute)
	require.NotEqual(t, first.Authorization, reissued.Authorization)
	_, err = session.ReserveApproval(t.Context(), files, reissued, authority.digest, now)
	require.ErrorIs(t, err, ErrApprovalAlreadyUsed, "token bytes are not the replay key")
	config.Audience = "rotated-authority-audience"
	installed, err := session.ConfigureApprovalAuthority(t.Context(), config, authority.digest)
	require.NoError(t, err)
	_, err = session.ReserveApproval(t.Context(), files, mint(installed.Digest, time.Minute), installed.Digest, now)
	require.ErrorIs(t, err, ErrApprovalAlreadyUsed, "policy rotation is not a replay reset")
	inspected, err := session.InspectApprovalUse(t.Context(), used.Approval.UseIdentity)
	require.NoError(t, err)
	require.Equal(t, used, inspected, "historical evidence remains readable under a different current policy")
}

func TestApprovalUseKeepsDamagedOrInterruptedRecordsSpent(t *testing.T) {
	session, files, approval, checked, now := approvalUseFixture(t)
	path, _, err := session.approvalUsePath(checked.UseIdentity)
	require.NoError(t, err)
	directory, err := openAuthorityRegistry(path, true)
	require.NoError(t, err)
	defer func() { require.NoError(t, directory.Close()) }()
	for _, contents := range [][]byte{nil, []byte("partial interrupted record")} {
		require.NoError(t, os.WriteFile(path, contents, 0o600))
		_, err = session.ReserveApproval(t.Context(), files, approval, checked.AuthorityDigest, now)
		require.ErrorIs(t, err, ErrApprovalAlreadyUsed)
		_, err = session.InspectApprovalUse(t.Context(), checked.UseIdentity)
		require.Error(t, err, "damage is not an unused approval")
		require.NoError(t, os.Remove(path))
	}
	require.NoError(t, os.Symlink(filepath.Join(filepath.Dir(path), "absent"), path))
	_, err = session.ReserveApproval(t.Context(), files, approval, checked.AuthorityDigest, now)
	require.ErrorIs(t, err, ErrApprovalAlreadyUsed, "dangling symlink still occupies the use identity")
	require.NoError(t, os.Remove(path))
	// A crash before publication leaves only an unrelated temporary file.
	require.NoError(t, directory.WriteFile(".approval-use-interrupted", []byte("partial"), 0o600))
	_, err = session.ReserveApproval(t.Context(), files, approval, checked.AuthorityDigest, now)
	require.NoError(t, err)
}

type approvalUseProcessInput struct {
	Workspace, Product, Configuration, Authority string
	Files                                        DeploymentFiles
	Approval                                     DeploymentApproval
	Now                                          time.Time
}

func TestApprovalUseProcess(t *testing.T) {
	path := os.Getenv("CODEFLY_TEST_APPROVAL_USE")
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var input approvalUseProcessInput
	require.NoError(t, json.Unmarshal(data, &input))
	session, err := NewSelectionSession(input.Workspace, input.Product, input.Configuration)
	require.NoError(t, err)
	_, err = session.ReserveApproval(t.Context(), &input.Files, &input.Approval, input.Authority, input.Now)
	if errors.Is(err, ErrApprovalAlreadyUsed) {
		os.Exit(42)
	}
	require.NoError(t, err)
	if os.Getenv("CODEFLY_TEST_APPROVAL_USE_LOSE_REPLY") == "1" {
		os.Exit(43)
	}
}

func TestApprovalUseSerializesRealProcessesAndSurvivesLostReply(t *testing.T) {
	for _, loseReply := range []bool{false, true} {
		t.Run(map[bool]string{false: "concurrent", true: "lost-reply"}[loseReply], func(t *testing.T) {
			session, files, approval, checked, now := approvalUseFixture(t)
			input := approvalUseProcessInput{Workspace: filepath.Dir(session.trustPath), Product: session.Root, Configuration: session.ConfigurationIdentity,
				Authority: checked.AuthorityDigest, Files: *files, Approval: *approval, Now: now}
			data, err := json.Marshal(input)
			require.NoError(t, err)
			path := filepath.Join(t.TempDir(), "input.json")
			require.NoError(t, os.WriteFile(path, data, 0o600))
			var commands []*exec.Cmd
			var outputs []*bytes.Buffer
			for range 3 {
				command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestApprovalUseProcess$")
				command.Env = append(os.Environ(), "CODEFLY_TEST_APPROVAL_USE="+path)
				if loseReply {
					command.Env = append(command.Env, "CODEFLY_TEST_APPROVAL_USE_LOSE_REPLY=1")
				}
				output := new(bytes.Buffer)
				command.Stdout, command.Stderr = output, output
				require.NoError(t, command.Start())
				commands = append(commands, command)
				outputs = append(outputs, output)
			}
			accepted, spent := 0, 0
			for i, command := range commands {
				err = command.Wait()
				if err == nil {
					accepted++
					continue
				}
				var exit *exec.ExitError
				require.ErrorAs(t, err, &exit, "%s", outputs[i])
				switch exit.ExitCode() {
				case 42:
					spent++
				case 43:
					require.True(t, loseReply)
					accepted++
				default:
					t.Fatalf("unexpected child failure: %v: %s", err, outputs[i])
				}
			}
			require.Equal(t, 1, accepted)
			require.Equal(t, 2, spent)
			_, err = session.ReserveApproval(t.Context(), files, approval, checked.AuthorityDigest, now)
			require.ErrorIs(t, err, ErrApprovalAlreadyUsed)
			used, err := session.InspectApprovalUse(t.Context(), checked.UseIdentity)
			require.NoError(t, err)
			require.Equal(t, *checked, used.Approval)
		})
	}
}

func TestApprovalUseRejectsUnsafeStorageAndInvalidIdentity(t *testing.T) {
	session, files, approval, checked, now := approvalUseFixture(t)
	for _, identity := range []string{"", "../authority", strings.ToUpper(checked.UseIdentity)} {
		_, err := session.InspectApprovalUse(t.Context(), identity)
		require.ErrorContains(t, err, "canonical SHA-256")
	}
	path, _, err := session.approvalUsePath(checked.UseIdentity)
	require.NoError(t, err)
	require.NoError(t, os.Mkdir(filepath.Dir(path), 0o700))
	require.NoError(t, os.Chmod(filepath.Dir(path), 0o777))
	_, err = session.ReserveApproval(t.Context(), files, approval, checked.AuthorityDigest, now)
	require.ErrorContains(t, err, "directory must not be group/world writable")
	_, err = os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
}
