package composition

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"maps"
	"path/filepath"
	"time"

	core "github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/policy"
)

const approvalAction = "composition.admission.approve"

// DeploymentApproval carries an existing Core record and scoped authorization,
// not another resolution/qualification model or an effect-consumption receipt.
type DeploymentApproval struct {
	Admission     AdmissionInspection `json:"admission"`
	Authorization string              `json:"authorization"`
}

type ApprovalOptions struct {
	ExpectedIdentity  string
	ExpectedAuthority string
	SigningKey        ed25519.PrivateKey
	ExpiresAt         time.Time
	Destination       string
}

type ApprovalInspection struct {
	AdmissionIdentity string    `json:"admissionIdentity"`
	AuthorityDigest   string    `json:"authorityDigest"`
	ApproverID        string    `json:"approverID"`
	ExpiresAt         time.Time `json:"expiresAt"`
}

// ApproveAdmission is a trusted-local signing operation. It accepts no policy,
// verification key or audience from the candidate. Host authority is loaded
// afresh and held stable under the selection lock through publication.
func (session *SelectionSession) ApproveAdmission(ctx context.Context, files *DeploymentFiles, recorded *AdmissionInspection, options *ApprovalOptions, now time.Time) (*ApprovalInspection, error) {
	started := time.Now()
	if options == nil || now.IsZero() || !filepath.IsAbs(options.Destination) {
		return nil, errors.New("approval requires explicit options and an absolute destination")
	}
	var result *ApprovalInspection
	err := session.withResolved(ctx, func(snapshot *selectionSnapshot, resolved *core.ResolvedComposition) error {
		if err := session.checkStagingParent(filepath.Dir(options.Destination), snapshot.local); err != nil {
			return err
		}
		authority, err := session.loadApprovalAuthority()
		if err != nil {
			return err
		}
		if options.ExpectedAuthority == "" || options.ExpectedAuthority != authority.digest {
			return errors.New("host approval authority differs from the independently reviewed authority digest")
		}
		if len(options.SigningKey) != ed25519.PrivateKeySize || !bytes.Equal(options.SigningKey[ed25519.SeedSize:], authority.config.Key) {
			return errors.New("signing key is not the host-configured approval authority")
		}
		checked, err := session.recheckResolved(ctx, resolved, files, authority.config.Policy, recorded, options.ExpectedIdentity, now.Add(time.Since(started)))
		if err != nil {
			return err
		}
		if !maps.Equal(checked.Current.Record.Bindings, authority.config.Bindings) {
			return errors.New("deployment target bindings differ from host approval authority")
		}
		issued := now.Add(time.Since(started))
		if options.ExpiresAt.Unix() <= issued.Unix() || options.ExpiresAt.After(checked.Current.Record.ValidUntil) {
			return errors.New("approval expiry must be future and no later than qualification validity")
		}
		token, _, err := policy.MintEd25519(policy.MintInput{
			Principal: &authority.config.Approver, Action: approvalAction, Resource: recorded.Identity,
			AudienceID: authority.config.Audience, CatalogDigest: authority.digest, RequestDigest: recorded.Record.BindingIdentity,
			TTL: options.ExpiresAt.Sub(issued), MaxUses: 1, NowFunc: func() time.Time { return issued },
		}, options.SigningKey)
		if err != nil {
			return err
		}
		approval := &DeploymentApproval{Admission: *recorded, Authorization: token}
		result, err = authority.verify(approval, now.Add(time.Since(started)))
		if err != nil {
			return err
		}
		data, err := json.Marshal(approval)
		if err != nil {
			return err
		}
		if err = session.unchanged(snapshot); err != nil {
			return err
		}
		if err = authority.unchanged(); err != nil {
			return err
		}
		return publishAdmissionRecord(ctx, options.Destination, data)
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// CheckApproval verifies evidence; it neither consumes MaxUses nor authorizes
// effects. Deployment owners still require durable fencing, isolated exact
// inputs, effect-time admission and their existing mutation authorization.
func (session *SelectionSession) CheckApproval(ctx context.Context, files *DeploymentFiles, approval *DeploymentApproval, now time.Time) (*ApprovalInspection, error) {
	started := time.Now()
	if now.IsZero() {
		return nil, errors.New("current time is required for approval verification")
	}
	var result *ApprovalInspection
	err := session.withResolved(ctx, func(snapshot *selectionSnapshot, resolved *core.ResolvedComposition) error {
		authority, err := session.loadApprovalAuthority()
		if err != nil {
			return err
		}
		result, err = authority.verify(approval, now.Add(time.Since(started)))
		if err != nil {
			return err
		}
		if _, err = session.recheckResolved(ctx, resolved, files, authority.config.Policy, &approval.Admission, result.AdmissionIdentity, now.Add(time.Since(started))); err != nil {
			return err
		}
		if err = session.unchanged(snapshot); err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = authority.unchanged(); err != nil {
			return err
		}
		result, err = authority.verify(approval, now.Add(time.Since(started)))
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (authority *approvalAuthority) verify(approval *DeploymentApproval, now time.Time) (*ApprovalInspection, error) {
	if authority == nil || approval == nil || now.IsZero() {
		return nil, errors.New("approval, host authority and current time are required")
	}
	if err := validateApprovalAuthority(&authority.config); err != nil {
		return nil, err
	}
	record := &approval.Admission
	if record.Identity == "" || authority.digest == "" || record.Record.BindingIdentity == "" || record.Record.ApprovedAt.IsZero() || record.Record.ValidUntil.IsZero() {
		return nil, errors.New("approval requires exact admission, policy and target identities with qualification validity")
	}
	if !maps.Equal(record.Record.Bindings, authority.config.Bindings) {
		return nil, errors.New("approval target bindings differ from host authority")
	}
	approver := &authority.config.Approver
	claims, err := policy.VerifyEd25519(approval.Authorization, policy.VerifyExpectations{
		Action: approvalAction, Resource: record.Identity, Audience: authority.config.Audience,
		CatalogDigest: authority.digest, RequestDigest: record.Record.BindingIdentity,
		PrincipalID: approver.ID, PrincipalKind: approver.Kind, OrganizationID: approver.OrgID, Now: now,
	}, authority.config.Key)
	if err != nil {
		return nil, errors.New("approval signature or host-owned authorization bindings are invalid")
	}
	// Core deliberately tolerates clock skew and does not consume MaxUses.
	// Inspection enforces literal validity but does not claim durable consumption.
	if claims.ID == "" || claims.MaxUses != 1 || claims.PrincipalOrgID != approver.OrgID ||
		claims.IssuedAtUnix > now.Unix() || claims.IssuedAtUnix < record.Record.ApprovedAt.Unix() || claims.ExpiresAtUnix <= claims.IssuedAtUnix ||
		!now.Before(time.Unix(claims.ExpiresAtUnix, 0)) || time.Unix(claims.ExpiresAtUnix, 0).After(record.Record.ValidUntil) {
		return nil, errors.New("approval is expired, future, outside qualification validity or has invalid use/identity claims")
	}
	return &ApprovalInspection{AdmissionIdentity: record.Identity, AuthorityDigest: authority.digest,
		ApproverID: approver.ID, ExpiresAt: time.Unix(claims.ExpiresAtUnix, 0).UTC()}, nil
}
