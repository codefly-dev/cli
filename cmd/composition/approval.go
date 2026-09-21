package composition

import (
	"crypto/ed25519"
	"errors"
	"os"
	"time"

	selection "github.com/codefly-dev/cli/pkg/composition"
	"github.com/spf13/cobra"
)

type approvalFlags struct {
	expected, authority, signingKey, expires string
}

func addApprovalCommands(add func(string, string, cobra.PositionalArgs, func(*cobra.Command, *selection.SelectionSession, []string) (any, error)) *cobra.Command) {
	var authorityDigest string
	configureAuthority := add("configure-approval-authority CONFIG.json", "Install host-owned approval policy, identity, key and target bindings", cobra.ExactArgs(1), func(cmd *cobra.Command, current *selection.SelectionSession, args []string) (any, error) {
		var config selection.ApprovalAuthorityConfig
		if err := readJSON(args[0], &config); err != nil {
			return nil, err
		}
		return current.ConfigureApprovalAuthority(cmd.Context(), &config, authorityDigest)
	})
	configureAuthority.Flags().StringVar(&authorityDigest, "expected-digest", "", "Current authority digest required for an explicit replacement")
	configureAuthority.Long = "Trusted-local administration only. Installs public authority configuration outside the workspace under CODEFLY_HOME. Replacing existing policy, key, audience or target bindings requires its current digest."
	configureAuthority.Example = "codefly composition configure-approval-authority authority.json --configuration config.json --identity-key identity.key"
	add("inspect-approval-authority", "Inspect the installed host approval authority identity", cobra.NoArgs, func(cmd *cobra.Command, current *selection.SelectionSession, _ []string) (any, error) {
		return current.InspectApprovalAuthority(cmd.Context())
	})
	var approval approvalFlags
	approve := add("approve-admission INPUTS.json RECORD.json DESTINATION.json", "Sign exact qualified admission using installed host authority; never deploy", cobra.ExactArgs(3), approval.run)
	approve.Long = "Rechecks the independently reviewed admission and actual files under installed host policy before signing with the configured approval key. Approval expires no later than qualification evidence. It does not run tests, consume authorization, apply or publish workloads."
	approve.Example = `codefly composition approve-admission inputs.json admission.json /private/review/approval.json --expected-identity "$ADMISSION_ID" --expected-authority "$AUTHORITY_DIGEST" --signing-key approval.key --expires "$EXPIRY" --render-requests requests.json --identity-key identity.key`
	approve.Flags().StringVar(&approval.expected, "expected-identity", "", "Independently reviewed and retained admission identity")
	approve.Flags().StringVar(&approval.authority, "expected-authority", "", "Independently reviewed and retained host authority digest")
	approve.Flags().StringVar(&approval.signingKey, "signing-key", "", "Private file containing the configured approver's 64 raw Ed25519 key bytes")
	approve.Flags().StringVar(&approval.expires, "expires", "", "Explicit RFC3339 approval expiry, bounded by qualification validity")
	for _, name := range []string{"expected-identity", "expected-authority", "signing-key", "expires"} {
		_ = approve.MarkFlagRequired(name)
	}
	add("check-approval INPUTS.json APPROVAL.json", "Verify approval, installed authority and fresh inputs without consuming or deploying", cobra.ExactArgs(2), checkApproval)
	inspectUse := add("inspect-approval-use ID", "Inspect a retained host approval-use record without implying deployment", cobra.ExactArgs(1), func(cmd *cobra.Command, current *selection.SelectionSession, args []string) (any, error) {
		return current.InspectApprovalUse(cmd.Context(), args[0])
	})
	inspectUse.Long = "Read historical single-use consumption evidence from protected host storage. A record does not prove deployment or health; an absent record does not prove that no effect occurred. No reset, retry or deployment authorization is provided."
}

func (flags *approvalFlags) run(cmd *cobra.Command, session *selection.SelectionSession, args []string) (any, error) {
	var inputs selection.DeploymentFiles
	var recorded selection.AdmissionInspection
	if err := readJSON(args[0], &inputs); err != nil {
		return nil, err
	}
	if err := readJSON(args[1], &recorded); err != nil {
		return nil, err
	}
	expires, err := time.Parse(time.RFC3339, flags.expires)
	if err != nil {
		return nil, errors.New("--expires must be an explicit RFC3339 timestamp")
	}
	key, err := readApprovalKey(flags.signingKey)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	return session.ApproveAdmission(cmd.Context(), &inputs, &recorded, &selection.ApprovalOptions{
		ExpectedIdentity: flags.expected, ExpectedAuthority: flags.authority, SigningKey: key, ExpiresAt: expires, Destination: args[2],
	}, time.Now())
}

func readApprovalKey(path string) (ed25519.PrivateKey, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open approval signing key")
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != ed25519.PrivateKeySize || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("approval signing key must be a private regular file containing 64 raw Ed25519 bytes")
	}
	key := make(ed25519.PrivateKey, ed25519.PrivateKeySize)
	if _, err = file.ReadAt(key, 0); err != nil {
		clear(key)
		return nil, errors.New("cannot read approval signing key")
	}
	return key, nil
}

func checkApproval(cmd *cobra.Command, session *selection.SelectionSession, args []string) (any, error) {
	var inputs selection.DeploymentFiles
	var approval selection.DeploymentApproval
	if err := readJSON(args[0], &inputs); err != nil {
		return nil, err
	}
	if err := readJSON(args[1], &approval); err != nil {
		return nil, err
	}
	return session.CheckApproval(cmd.Context(), &inputs, &approval, time.Now())
}
