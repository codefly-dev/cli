package control

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// This file lifts the MutationAuthority group. It is the transport-agnostic gate
// underneath the Gateway's ConfigureMutationAuthority/PrepareMutation/
// ApplyPreparedMutation. The Gateway's heavyweight remote protocol (Ed25519
// permits, fence tokens, coordinator-key/workspace pinning) is network-trust
// machinery that belongs in the GATEWAY ADAPTER, at the boundary Mind talks to.
// Here the plane only enforces the local policy: in "open" mode mutations apply
// directly; in "prepared" mode a mutation must be prepared (yielding an opaque,
// single-use, expiring token) and then applied with that token.

// preparedMutationLifetime bounds how long a prepared token stays valid.
const preparedMutationLifetime = 5 * time.Minute

// mutationGate holds the authority mode and any outstanding prepared mutations.
type mutationGate struct {
	mu      sync.Mutex
	mode    AuthorityMode
	pending map[string]pendingMutation
}

type pendingMutation struct {
	mutation  Mutation
	expiresAt time.Time
}

// newMutationGate defaults to open — a locally-run CLI is trusted; a remote
// caller (Mind, via the Gateway) tightens it to prepared.
func newMutationGate() *mutationGate {
	return &mutationGate{mode: AuthorityOpen, pending: map[string]pendingMutation{}}
}

func (g *mutationGate) currentMode() AuthorityMode {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.mode
}

func (p *planeImpl) ConfigureMutationAuthority(ctx context.Context, cfg AuthorityConfig) error {
	switch cfg.Mode {
	case AuthorityOpen, AuthorityPrepared:
	default:
		return fmt.Errorf("unknown mutation authority mode %q", cfg.Mode)
	}
	p.gate.mu.Lock()
	defer p.gate.mu.Unlock()
	p.gate.mode = cfg.Mode
	// Re-pinning authority invalidates any outstanding prepared mutations.
	p.gate.pending = map[string]pendingMutation{}
	return nil
}

func (p *planeImpl) PrepareMutation(ctx context.Context, m Mutation) (PreparedMutation, error) {
	if err := validateMutation(m); err != nil {
		return PreparedMutation{}, err
	}
	token, err := newMutationToken()
	if err != nil {
		return PreparedMutation{}, err
	}
	p.gate.mu.Lock()
	defer p.gate.mu.Unlock()
	p.gate.pending[token] = pendingMutation{mutation: m, expiresAt: time.Now().Add(preparedMutationLifetime)}
	return PreparedMutation{Token: token}, nil
}

func (p *planeImpl) ApplyPreparedMutation(ctx context.Context, token PreparedMutation) error {
	p.gate.mu.Lock()
	pending, ok := p.gate.pending[token.Token]
	if ok {
		// Single-use: consume regardless of the apply outcome so a failed apply
		// can't be retried against a stale token.
		delete(p.gate.pending, token.Token)
	}
	p.gate.mu.Unlock()

	if !ok {
		return fmt.Errorf("unknown or already-consumed prepared mutation")
	}
	if time.Now().After(pending.expiresAt) {
		return fmt.Errorf("prepared mutation expired")
	}
	return p.executeMutation(ctx, pending.mutation)
}

// executeMutation dispatches a prepared mutation to the underlying operation.
func (p *planeImpl) executeMutation(ctx context.Context, m Mutation) error {
	switch m.Kind {
	case MutationFile:
		edit, ok := m.Payload.(Edit)
		if !ok {
			return fmt.Errorf("file mutation payload must be an Edit, got %T", m.Payload)
		}
		return p.ApplyEdit(ctx, edit)
	case MutationDeploy:
		req, ok := m.Payload.(DeployRequest)
		if !ok {
			return fmt.Errorf("deploy mutation payload must be a DeployRequest, got %T", m.Payload)
		}
		_, err := p.runDeploy(ctx, req)
		return err
	default:
		return fmt.Errorf("unsupported mutation kind %q", m.Kind)
	}
}

// validateMutation checks the payload type matches the kind before a token is
// issued, so a malformed mutation fails at prepare time, not apply time.
func validateMutation(m Mutation) error {
	switch m.Kind {
	case MutationFile:
		if _, ok := m.Payload.(Edit); !ok {
			return fmt.Errorf("file mutation payload must be an Edit, got %T", m.Payload)
		}
	case MutationDeploy:
		if _, ok := m.Payload.(DeployRequest); !ok {
			return fmt.Errorf("deploy mutation payload must be a DeployRequest, got %T", m.Payload)
		}
	default:
		return fmt.Errorf("unsupported mutation kind %q", m.Kind)
	}
	return nil
}

// newMutationToken returns an opaque, unguessable single-use token.
func newMutationToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate mutation token: %w", err)
	}
	return hex.EncodeToString(b), nil
}
