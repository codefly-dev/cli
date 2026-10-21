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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/codefly-dev/core/workloadidentity"
	"github.com/codefly-dev/core/workloadidentity/native"
)

// Credential is one projected identity: the workload it names, the container
// it authenticates as, and the file the process reads it from.
type Credential struct {
	Workload  workloadidentity.Workload
	Container string
	File      string
}

// Launcher issues the run's credentials. One per run; it dies with it.
type Launcher struct {
	issuer      *native.Issuer
	trustDomain string
	audience    string
	stateDir    string
	runID       string

	mu          sync.Mutex
	credentials map[projection]Credential // active refresh only
	cleanup     map[string]bool           // retired paths retained until removal succeeds
	closed      bool
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
	return &Launcher{issuer: issuer, trustDomain: trustDomain, audience: audience, stateDir: stateDir, runID: randomID(), credentials: map[projection]Credential{}}, nil
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

type projection struct {
	Unique   string
	Audience string
}

// Project preserves the implicit host-audience projection.
func (l *Launcher) Project(unique string, workload workloadidentity.Workload) (Credential, error) {
	return l.ProjectForAudience(unique, workload, l.audience)
}

// ProjectForAudience projects one declared workload for one audience. Every
// audience shares the workload instance, but reads a separate private file.
func (l *Launcher) ProjectForAudience(unique string, workload workloadidentity.Workload, audience string) (Credential, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return Credential{}, fmt.Errorf("native identity launcher is closed")
	}
	if unique != workload.Unique() || workload.TrustDomain != l.trustDomain || workload.Instance != "" {
		return Credential{}, fmt.Errorf("projection %q must name its declared workload under this run's trust domain", unique)
	}
	if audience == "" {
		return Credential{}, fmt.Errorf("native projection requires an audience")
	}
	for _, part := range []string{workload.Module, workload.Service} {
		if part == "." || part == ".." || part == "" || strings.ContainsAny(part, "/\\\x00") {
			return Credential{}, fmt.Errorf("native workload name %q is not a safe path component", part)
		}
	}
	key := projection{unique, audience}
	if credential, done := l.credentials[key]; done {
		return credential, nil
	}
	workload.Instance = workload.Service + "-" + l.runID
	// Preserve the host file path. Additional audiences are opaque, so only
	// their full digest enters the generated path, under the same workload.
	path := filepath.Join(l.stateDir, "identities", workload.Module, workload.Service, "credential")
	if audience != l.audience {
		audienceKey := sha256.Sum256([]byte(audience))
		path = filepath.Join(filepath.Dir(path), "audiences", hex.EncodeToString(audienceKey[:]), "credential")
	}
	if l.cleanup[path] {
		if err := l.removeCredential(path); err != nil {
			return Credential{}, err
		}
	}
	credential := Credential{Workload: workload, Container: workload.Service, File: path}
	if err := l.issuer.WriteCredential(credential.File, l.subject(&credential), audience); err != nil {
		return Credential{}, err
	}
	l.credentials[key] = credential
	return credential, nil
}

// Refresh re-issues each projection under its original audience.
func (l *Launcher) Refresh() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	var failures []error
	for key, credential := range l.credentials {
		if err := l.issuer.WriteCredential(credential.File, l.subject(&credential), key.Audience); err != nil {
			failures = append(failures, fmt.Errorf("refresh %s for %q: %w", key.Unique, key.Audience, err))
		}
	}
	return errors.Join(failures...)
}

// RefreshInterval is how often Refresh runs.
const RefreshInterval = native.RefreshInterval

// Forget retires every audience before attempting removal. Failed paths stay
// pending, never active: Refresh cannot recreate a stopped workload's files.
func (l *Launcher) Forget(unique string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.retire(unique)
	return l.cleanupFiles()
}

func (l *Launcher) retire(unique string) {
	if l.cleanup == nil {
		l.cleanup = map[string]bool{}
	}
	for key, credential := range l.credentials {
		if unique != "" && key.Unique != unique {
			continue
		}
		delete(l.credentials, key)
		l.cleanup[credential.File] = true
	}
}

// RetryCleanup retries retired files without reactivating their credentials.
func (l *Launcher) RetryCleanup() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cleanupFiles()
}

func (l *Launcher) cleanupFiles() error {
	var failures []error
	for path := range l.cleanup {
		if err := l.removeCredential(path); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (l *Launcher) removeCredential(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove native credential %s: %w", path, err)
	}
	delete(l.cleanup, path)
	return nil
}

// Close prevents further projections, retires all credentials and retries any
// pending removals. It leaves unrelated run state intact. Copied tokens expire;
// deleting a projected file does not revoke bytes already read by a process.
func (l *Launcher) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	l.retire("")
	return l.cleanupFiles()
}

// Projected reports whether unique has a credential.
func (l *Launcher) Projected(unique string) (Credential, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	credential, ok := l.credentials[projection{unique, l.audience}]
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
