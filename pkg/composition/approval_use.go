package composition

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	core "github.com/codefly-dev/core/composition"
)

var (
	ErrApprovalAlreadyUsed  = errors.New("approval authorization has already been consumed; never retry deployment with this token")
	ErrApprovalUseUncertain = errors.New("approval authorization may be consumed; inspect the retained use record and reconcile effects, never automatically retry")
)

const approvalUseSchema = "codefly/approval-use/v1"

// ApprovalUse records only local-host authorization consumption, not a
// deployment, successful effect, observed state or target-wide fence.
type ApprovalUse struct {
	Schema          string              `json:"schema"`
	ProductIdentity string              `json:"productIdentity"`
	Approval        ApprovalInspection  `json:"approval"`
	Admission       AdmissionInspection `json:"admission"`
	ConsumedAt      time.Time           `json:"consumedAt"`
}

func approvalUseIdentity(key ed25519.PublicKey, id string) string {
	// Scope by signer and token ID, not mutable policy or token encoding. A
	// policy rotation or a differently encoded signature must not reset use.
	data, _ := json.Marshal(struct {
		Key ed25519.PublicKey `json:"key"`
		ID  string            `json:"id"`
	}{key, id})
	return contentDigest(data)
}

// ReserveApproval is an effect-owner primitive, deliberately not exposed as a
// deploy command. It re-admits current inputs and durably spends one token.
// It does not isolate bytes, fence a target, or grant an effect-path bypass.
// A caller must never release a use on cancellation or an uncertain outcome.
func (session *SelectionSession) ReserveApproval(ctx context.Context, files *DeploymentFiles, approval *DeploymentApproval, expectedAuthority string, now time.Time) (*ApprovalUse, error) {
	started := time.Now()
	if now.IsZero() || expectedAuthority == "" {
		return nil, errors.New("approval consumption requires current time and an independently reviewed host authority digest")
	}
	var used *ApprovalUse
	err := session.withResolved(ctx, func(snapshot *selectionSnapshot, resolved *core.ResolvedComposition) error {
		authority, err := session.loadApprovalAuthority()
		if err != nil {
			return err
		}
		if expectedAuthority != authority.digest {
			return errors.New("host approval authority differs from the independently reviewed authority digest")
		}
		verified, err := authority.verify(approval, now.Add(time.Since(started)))
		if err != nil {
			return err
		}
		checked, err := session.recheckResolved(ctx, resolved, files, authority.config.Policy, &approval.Admission, verified.AdmissionIdentity, now.Add(time.Since(started)))
		if err != nil {
			return err
		}
		path, product, err := session.approvalUsePath(verified.UseIdentity)
		if err != nil {
			return err
		}
		directory, err := openAuthorityRegistry(path, true)
		if err != nil {
			return err
		}
		defer func() { _ = directory.Close() }()
		if err = session.unchanged(ctx, snapshot); err != nil {
			return err
		}
		if err = authority.unchanged(); err != nil {
			return err
		}
		consumedAt := now.Add(time.Since(started))
		verified, err = authority.verify(approval, consumedAt)
		if err != nil {
			return err
		}
		used = &ApprovalUse{Schema: approvalUseSchema, ProductIdentity: product, Approval: *verified, Admission: checked.Current, ConsumedAt: consumedAt.UTC()}
		data, err := json.Marshal(used)
		if err != nil {
			return err
		}
		if err = publishApprovalUse(ctx, directory, filepath.Base(path), data); err != nil {
			return err
		}
		// Expiry, cancellation or drift after committing cannot unspend a token.
		if err = errors.Join(ctx.Err(), session.unchanged(ctx, snapshot), authority.unchanged()); err != nil {
			return errors.Join(ErrApprovalUseUncertain, err)
		}
		if _, err = authority.verify(approval, now.Add(time.Since(started))); err != nil {
			return errors.Join(ErrApprovalUseUncertain, err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return used, nil
}

func (session *SelectionSession) approvalUsePath(identity string) (string, string, error) {
	name, ok := strings.CutPrefix(identity, "sha256:")
	digest, err := hex.DecodeString(name)
	if !ok || err != nil || len(digest) != 32 || name != strings.ToLower(name) {
		return "", "", errors.New("approval use identity must be a canonical SHA-256 digest")
	}
	directory, product, err := session.approvalStorageDirectory("composition-approval-uses")
	if err != nil {
		return "", "", err
	}
	return filepath.Join(directory, name+".json"), product, nil
}

func (session *SelectionSession) approvalStorageDirectory(name string) (string, string, error) {
	path, err := session.approvalAuthorityPath()
	if err != nil {
		return "", "", err
	}
	directory := filepath.Join(filepath.Dir(filepath.Dir(path)), name)
	// Recheck the new storage directory against independent local checkouts too.
	local := map[string]string{}
	data, err := os.ReadFile(filepath.Join(session.Root, LocalSelectionFile))
	if err != nil && !os.IsNotExist(err) {
		return "", "", err
	}
	if err == nil {
		if err = decodeSelectionJSON(data, &local); err != nil || local == nil {
			return "", "", errors.New("local selections must be a valid object")
		}
	}
	if err = session.checkApprovalStorage(directory, local); err != nil {
		return "", "", err
	}
	product := "sha256:" + strings.TrimSuffix(filepath.Base(path), ".json")
	return directory, product, nil
}

// InspectApprovalUse is historical evidence, independent of current key
// rotation and expiry. Absence is not proof that an effect did not occur.
func (session *SelectionSession) InspectApprovalUse(ctx context.Context, identity string) (*ApprovalUse, error) {
	var used ApprovalUse
	err := session.locked(ctx, func() error {
		path, product, err := session.approvalUsePath(identity)
		if err != nil {
			return err
		}
		data, err := readAuthorityDocument(path)
		if err != nil {
			return err
		}
		if err = decodeSelectionJSON(data, &used); err != nil {
			return err
		}
		if used.Schema != approvalUseSchema || used.ProductIdentity != product || used.Approval.UseIdentity != identity || used.ConsumedAt.IsZero() ||
			used.Approval.AdmissionIdentity == "" || used.Approval.AuthorityDigest == "" || used.Admission.Identity == "" {
			return errors.New("approval use record is invalid or belongs to another product; consumption must not be reset")
		}
		return ctx.Err()
	})
	if err != nil {
		return nil, err
	}
	return &used, nil
}

func publishApprovalUse(ctx context.Context, directory *os.Root, name string, data []byte) (resultErr error) {
	if len(data) > 1<<20 {
		return errors.New("approval use record exceeds size limit")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	temporary := ".approval-use-" + rand.Text()
	file, err := directory.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	linked := false
	defer func() {
		resultErr = finalizeApprovalUse(ctx, directory, temporary, linked, resultErr)
	}()
	if err = validateAuthorityAccess(file); err != nil {
		return errors.Join(err, file.Close())
	}
	_, writeErr := file.Write(data)
	if err = errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = directory.Link(temporary, name); err != nil {
		if errors.Is(err, os.ErrExist) {
			return ErrApprovalAlreadyUsed
		}
		return errors.Join(ErrApprovalUseUncertain, fmt.Errorf("publish approval use without replacing records: %w", err))
	}
	linked = true
	return nil
}

// Once linked, neither a failed sync nor failed cleanup can refund the use.
// Check cancellation after cleanup as well as before publication.
func finalizeApprovalUse(ctx context.Context, directory *os.Root, temporary string, linked bool, resultErr error) error {
	if linked {
		resultErr = errors.Join(resultErr, syncAuthorityDirectory(directory))
	}
	resultErr = errors.Join(resultErr, directory.Remove(temporary), ctx.Err())
	if linked && resultErr != nil {
		return errors.Join(ErrApprovalUseUncertain, resultErr)
	}
	return resultErr
}
