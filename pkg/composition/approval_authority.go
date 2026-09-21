package composition

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	core "github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/policy"
	"github.com/codefly-dev/core/resources"
)

// ApprovalAuthorityConfig is host configuration, never a candidate-supplied
// policy. Core owns qualification policy, identity claims and token verification.
type ApprovalAuthorityConfig struct {
	Audience string                `json:"audience"`
	Approver policy.Principal      `json:"approver"`
	Key      ed25519.PublicKey     `json:"key"`
	Policy   core.DeploymentPolicy `json:"policy"`
	Bindings map[string]string     `json:"bindings"`
}

type ApprovalAuthorityInspection struct {
	Digest string `json:"digest"`
	Path   string `json:"path"`
}

type approvalAuthority struct {
	config   ApprovalAuthorityConfig
	digest   string
	path     string
	home     string
	document []byte
}

func validateApprovalAuthority(config *ApprovalAuthorityConfig) error {
	if config == nil || config.Audience == "" || config.Audience != strings.TrimSpace(config.Audience) {
		return errors.New("approval authority requires an explicit canonical audience")
	}
	if err := config.Approver.Validate(); err != nil {
		return err
	}
	if config.Approver.ID != strings.TrimSpace(config.Approver.ID) || config.Approver.OrgID != strings.TrimSpace(config.Approver.OrgID) {
		return errors.New("approver identity must be canonical")
	}
	if config.Approver.Token != "" || len(config.Approver.DelegationChain) != 0 || !config.Approver.ExpiresAt.IsZero() {
		return errors.New("configure the approver identity, not credentials or an unverified delegation")
	}
	if len(config.Key) != ed25519.PublicKeySize {
		return errors.New("approval authority requires an Ed25519 public key")
	}
	if len(config.Bindings) == 0 {
		return errors.New("approval authority requires explicit target bindings")
	}
	for name, digest := range config.Bindings {
		value, ok := strings.CutPrefix(digest, "sha256:")
		decoded, err := hex.DecodeString(value)
		if name == "" || name != strings.TrimSpace(name) || !ok || err != nil || len(decoded) != 32 || value != strings.ToLower(value) {
			return errors.New("approval target bindings require canonical names and SHA-256 identities")
		}
	}
	return validateApprovalPolicy(&config.Policy)
}

func validateApprovalPolicy(deploymentPolicy *core.DeploymentPolicy) error {
	if !slices.Contains(deploymentPolicy.RequiredQualifications, "functional") {
		return errors.New("approval policy must explicitly require functional qualification")
	}
	seen := make(map[string]bool)
	for _, kind := range deploymentPolicy.RequiredQualifications {
		if kind == "" || kind != strings.TrimSpace(kind) || seen[kind] {
			return errors.New("approval policy requires unique canonical qualification kinds")
		}
		seen[kind] = true
		if len(deploymentPolicy.QualificationSigners[kind]) == 0 {
			return fmt.Errorf("required %s qualification has no configured authority", kind)
		}
	}
	for kind, signers := range deploymentPolicy.QualificationSigners {
		if kind == "" || kind != strings.TrimSpace(kind) || len(signers) == 0 {
			return errors.New("qualification signer scopes must be nonempty and canonical")
		}
		for name, key := range signers {
			if name == "" || name != strings.TrimSpace(name) || len(key) != ed25519.PublicKeySize {
				return errors.New("qualification authorities require canonical names and Ed25519 public keys")
			}
		}
	}
	return nil
}

func (session *SelectionSession) approvalAuthorityPath() (string, error) {
	root, err := filepath.EvalSymlinks(session.Root)
	if err != nil {
		return "", err
	}
	home, err := filepath.Abs(resources.CodeflyHomeDir())
	if err != nil {
		return "", err
	}
	home, err = canonicalAuthorityHome(home)
	if err != nil {
		return "", fmt.Errorf("approval authority requires a trusted existing host CODEFLY_HOME directory: %w", err)
	}
	local := make(map[string]string)
	data, err := os.ReadFile(filepath.Join(session.Root, LocalSelectionFile))
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	if err == nil {
		if err = decodeSelectionJSON(data, &local); err != nil {
			return "", err
		}
		if local == nil {
			return "", errors.New("local selections must be an object, not null")
		}
	}
	directory := filepath.Join(home, "composition-authorities")
	for _, storage := range []string{home, directory} {
		if err = session.checkApprovalStorage(storage, local); err != nil {
			return "", err
		}
	}
	return filepath.Join(directory, strings.TrimPrefix(contentDigest([]byte(root)), "sha256:")+".json"), nil
}

func (session *SelectionSession) checkApprovalStorage(directory string, local map[string]string) error {
	// Check both the host root and its generated storage directory: a checkout
	// can itself be named composition-authorities below an otherwise valid home.
	if err := session.checkStagingParent(directory, local); err != nil {
		return errors.New("approval authority storage must be outside the product and local checkouts")
	}
	if session.trustPath != "" {
		workspace, resolveErr := filepath.EvalSymlinks(filepath.Dir(session.trustPath))
		if resolveErr != nil {
			return resolveErr
		}
		relative, relErr := filepath.Rel(workspace, directory)
		if relErr != nil {
			return relErr
		}
		if filepath.IsLocal(relative) {
			return errors.New("approval authority storage resolves inside the workspace")
		}
	}
	return nil
}

// ConfigureApprovalAuthority is an explicit trusted-local administration action.
// Replacing policy/key requires the exact prior digest, never a force/default.
func (session *SelectionSession) ConfigureApprovalAuthority(ctx context.Context, config *ApprovalAuthorityConfig, expected string) (*ApprovalAuthorityInspection, error) {
	data, err := normalizedApprovalAuthority(config)
	if err != nil {
		return nil, err
	}
	var inspection *ApprovalAuthorityInspection
	err = session.locked(ctx, func() error {
		path, pathErr := session.approvalAuthorityPath()
		if pathErr != nil {
			return pathErr
		}
		directory, openErr := openAuthorityRegistry(path, true)
		if openErr != nil {
			return openErr
		}
		defer func() { _ = directory.Close() }()
		before, readErr := readAuthorityDocumentAt(directory, filepath.Base(path))
		if readErr != nil && !os.IsNotExist(readErr) {
			return readErr
		}
		if readErr == nil {
			var previous ApprovalAuthorityConfig
			if decodeErr := decodeSelectionJSON(before, &previous); decodeErr != nil {
				return errors.New("existing approval authority is invalid; explicit host repair is required")
			}
			before, readErr = normalizedApprovalAuthority(&previous)
			if readErr != nil {
				return readErr
			}
		}
		if (readErr == nil && (expected == "" || expected != contentDigest(before))) || (os.IsNotExist(readErr) && expected != "") {
			return errors.New("approval authority changed or already exists; explicit current digest is required")
		}
		if writeErr := writeAuthorityDocumentAt(ctx, directory, filepath.Base(path), data); writeErr != nil {
			return writeErr
		}
		inspection = &ApprovalAuthorityInspection{Digest: contentDigest(data), Path: path}
		return nil
	})
	return inspection, err
}

func (session *SelectionSession) loadApprovalAuthority() (*approvalAuthority, error) {
	path, err := session.approvalAuthorityPath()
	if err != nil {
		return nil, err
	}
	if err = protectedAuthorityDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	}
	data, err := readAuthorityDocument(path)
	if err != nil {
		return nil, err
	}
	home, err := filepath.Abs(resources.CodeflyHomeDir())
	if err != nil {
		return nil, err
	}
	authority := &approvalAuthority{path: path, home: home, document: data}
	if err = decodeSelectionJSON(data, &authority.config); err != nil {
		return nil, errors.New("invalid approval authority configuration")
	}
	normalized, err := normalizedApprovalAuthority(&authority.config)
	if err != nil {
		return nil, err
	}
	authority.digest = contentDigest(normalized)
	return authority, nil
}

func (session *SelectionSession) InspectApprovalAuthority(ctx context.Context) (*ApprovalAuthorityInspection, error) {
	var result *ApprovalAuthorityInspection
	err := session.locked(ctx, func() error {
		authority, err := session.loadApprovalAuthority()
		if err != nil {
			return err
		}
		result = &ApprovalAuthorityInspection{Digest: authority.digest, Path: authority.path}
		return nil
	})
	return result, err
}

func normalizedApprovalAuthority(config *ApprovalAuthorityConfig) ([]byte, error) {
	if err := validateApprovalAuthority(config); err != nil {
		return nil, err
	}
	normalized := *config
	normalized.Policy.RequiredQualifications = slices.Clone(config.Policy.RequiredQualifications)
	slices.Sort(normalized.Policy.RequiredQualifications)
	return json.Marshal(normalized)
}

func (authority *approvalAuthority) unchanged() error {
	home, err := canonicalAuthorityHome(authority.home)
	if err != nil {
		return err
	}
	if home != filepath.Dir(filepath.Dir(authority.path)) {
		return errors.New("approval authority home changed during admission")
	}
	if err := protectedAuthorityDirectory(filepath.Dir(authority.path)); err != nil {
		return err
	}
	data, err := readAuthorityDocument(authority.path)
	if err != nil {
		return err
	}
	if !bytes.Equal(data, authority.document) {
		return errors.New("approval authority changed during admission; reopen and review current policy")
	}
	return nil
}

func protectedAuthorityDirectory(path string) error {
	directory, err := openProtectedAuthorityDirectory(path)
	if err != nil {
		return err
	}
	return directory.Close()
}

func readAuthorityDocument(path string) ([]byte, error) {
	directory, err := openAuthorityRegistry(path, false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = directory.Close() }()
	return readAuthorityDocumentAt(directory, filepath.Base(path))
}
