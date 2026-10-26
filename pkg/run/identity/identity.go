// Package identity is the run's half of workload identity: the launcher.
//
// A process started by `codefly run` has one identity, the declared workload
// (trust domain, module, service) the composition names it as — the same
// CODEFLY__MODULE / CODEFLY__SERVICE it is started with. The run is the one
// party that knows which workload each process is, so the run vouches: it
// signs a credential naming that workload and hands the host its anchor. No
// namespace, no account, no pod; those are a cell's spelling of the same
// declaration, and a cell proves them its own way.
package identity

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/codefly-dev/core/workloadidentity"
	"github.com/codefly-dev/core/workloadidentity/native"
)

// Credential is one projected identity: the workload it names, the container
// it authenticates as, and the file the process reads it from.
type Credential struct {
	Workload  workloadidentity.Workload
	Container string
	File      string
	expiresAt time.Time
}

// Launcher issues the run's credentials. One per run; it dies with it.
type Launcher struct {
	issuer      *native.Issuer
	trustDomain string
	audience    string
	stateDir    string
	runID       string

	mu          sync.Mutex
	credentials map[string]Credential // unique -> projected credential
}

// Start creates the run's issuer. trustDomain is the environment's, audience
// the host's bare audience, stateDir where credentials are projected.
func Start(trustDomain, audience, stateDir string) (*Launcher, error) {
	if trustDomain == "" || audience == "" {
		return nil, fmt.Errorf("a run vouches for workloads under a trust domain and for an audience, and the environment's host declares trust_domain %q, audience %q", trustDomain, audience)
	}
	issuer, err := native.NewIssuer()
	if err != nil {
		return nil, err
	}
	return &Launcher{issuer: issuer, trustDomain: trustDomain, audience: audience, stateDir: stateDir, runID: randomID(), credentials: map[string]Credential{}}, nil
}

// Workload is the declared workload of module/service under this run's trust
// domain.
func (l *Launcher) Workload(module, service string) workloadidentity.Workload {
	return workloadidentity.Workload{TrustDomain: l.trustDomain, Module: module, Service: service}
}

// Declare records workloads this run vouches for: an identity workload it
// starts, that module's delivery carrier, and the applied observer. The
// anchor written afterwards carries them; the host refuses anything else
// under this run's key.
func (l *Launcher) Declare(workloads ...workloadidentity.Workload) {
	l.issuer.Declare(workloads...)
}

// DeclareModule declares a module's identity service and its delivery carrier.
func (l *Launcher) DeclareModule(module, service string) {
	l.Declare(l.Workload(module, service), l.Workload(module, workloadidentity.DeliveryService))
}

// Observer is the applied-state observer's workload under this run.
func (l *Launcher) Observer() workloadidentity.Workload {
	return l.Workload(workloadidentity.AppliedObserver.Module, workloadidentity.AppliedObserver.Service)
}

// Anchor is what the host verifies this run's credentials under.
func (l *Launcher) Anchor() native.Anchor { return l.issuer.Anchor() }

// AnchorFingerprint is the anchor's key fingerprint, stable across Declares.
func (l *Launcher) AnchorFingerprint() string { return l.issuer.Anchor().Fingerprint() }

// WriteAnchor writes the anchor, with every workload declared so far, to path.
// Called again after each Declare: the host re-reads it.
func (l *Launcher) WriteAnchor(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return l.issuer.Anchor().WriteFile(path)
}

// ProjectionPath resolves the credential carrier without issuing or refreshing
// authority. Init may prepare the environment before a process is started.
func (l *Launcher) ProjectionPath(workload workloadidentity.Workload) string {
	return filepath.Join(l.stateDir, "identities", workload.Module, workload.Service, "credential")
}

// Project issues the credential of unique's workload once, to a file the
// process reads, and returns it. A second Project of the same unique returns
// the same credential.
func (l *Launcher) Project(unique string, workload workloadidentity.Workload) (Credential, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if credential, done := l.credentials[unique]; done {
		return credential, nil
	}
	workload.Instance = workload.Service + "-" + l.runID
	credential := Credential{
		Workload: workload, Container: workload.Service,
		File: l.ProjectionPath(workload),
	}
	if err := l.writeCredential(&credential); err != nil {
		return Credential{}, err
	}
	l.credentials[unique] = credential
	return credential, nil
}

// writeCredential records a conservative wall-clock expiry before issuance.
// Core owns the lifetime and atomic write; failed writes never advance expiry.
func (l *Launcher) writeCredential(credential *Credential) error {
	expiresAt := time.Now().Truncate(time.Second).Add(native.TokenLifetime)
	if err := l.issuer.WriteCredential(credential.File, l.subject(credential), l.audience); err != nil {
		return err
	}
	credential.expiresAt = expiresAt
	return nil
}

// Refresh re-issues projections with at most 20% of their lifetime remaining.
// now is sampled at the poll, not taken from a possibly stale ticker event.
func (l *Launcher) Refresh(now time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	var failures []error
	for unique, credential := range l.credentials {
		// Strip monotonic time: on platforms where it pauses during suspend,
		// only the wall clock includes the time the laptop spent asleep.
		if credential.expiresAt.Sub(now.Round(0)) > native.TokenLifetime-native.RefreshInterval {
			continue
		}
		if err := l.writeCredential(&credential); err != nil {
			failures = append(failures, fmt.Errorf("refresh workload credential %s: %w", unique, err))
			continue
		}
		l.credentials[unique] = credential
	}
	return errors.Join(failures...)
}

// Forget drops a stopped workload's projection.
func (l *Launcher) Forget(unique string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.credentials, unique)
}

// Projected reports whether unique has a credential.
func (l *Launcher) Projected(unique string) (Credential, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	credential, ok := l.credentials[unique]
	return credential, ok
}

// Issue is a one-off credential for a workload the run itself acts as: a
// module's delivery carrier, the applied observer. It names no container.
func (l *Launcher) Issue(workload workloadidentity.Workload) (string, error) {
	workload.Instance = "run-" + l.runID
	return l.issuer.Issue(native.Subject{Workload: workload}, l.audience)
}

func (l *Launcher) subject(credential *Credential) native.Subject {
	return native.Subject{Workload: credential.Workload, Container: credential.Container}
}

func randomID() string {
	raw := make([]byte, 8)
	_, _ = rand.Read(raw)
	return hex.EncodeToString(raw)
}
