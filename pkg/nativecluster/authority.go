// Package nativecluster is the workload identity a native `codefly run` gives
// the processes it starts.
//
// A host accepts a delivery and mints a workload credential only on what the
// environment attests about the caller. In a cell that is the cluster: a
// projected ServiceAccount token and the pod it names. A native run has no
// cluster and imitates none. The CLI starts every process, so it is the one
// party that knows which declared workload a process is and which build it
// launched, and it says so in its own credential: core's
// workloadidentity/native. The host verifies that credential against the
// run's anchor, which the CLI names to it in CODEFLY_NATIVE_IDENTITY_ANCHOR.
//
// Transport identity is not here. In a cell the mesh carries it; a laptop has
// no mesh to stand in for: every hop is plain HTTP on loopback.
package nativecluster

import (
	"context"
	"os"
	"path/filepath"
	"sync"

	"github.com/codefly-dev/core/workloadidentity/native"
)

// TokenRefreshInterval is how often a credential is reissued: at 80% of its
// lifetime, as a kubelet rotates a projected token. A mint client that re-reads
// its credential before every mint therefore always presents a fresh one.
const TokenRefreshInterval = native.RefreshInterval

// Pod is one native process as the run vouches for it: the identity its
// credential names and the build it runs, once known.
type Pod struct {
	Namespace      string
	ServiceAccount string
	Name           string
	UID            string
	Container      string
	Repository     string
	Digest         string
}

// Authority is the run's workload-identity issuer and what it knows about
// each process it started.
type Authority struct {
	issuer *native.Issuer

	mu   sync.Mutex
	pods map[string]*Pod
}

// Start makes the run's issuer. Its key is the run's and is never written:
// the host's local profile is bound to the anchor's fingerprint, so a new run
// is a new anchor, as a new cluster is a new CA.
func Start(_ context.Context) (*Authority, error) {
	issuer, err := native.NewIssuer()
	if err != nil {
		return nil, err
	}
	return &Authority{issuer: issuer, pods: map[string]*Pod{}}, nil
}

// Close releases nothing: the issuer holds no listener.
func (a *Authority) Close() error { return nil }

// Anchor is the public half the host verifies every credential against.
func (a *Authority) Anchor() native.Anchor { return a.issuer.Anchor() }

// AnchorFingerprint is what the host's local profile pins.
func (a *Authority) AnchorFingerprint() string { return a.issuer.Anchor().Fingerprint() }

func podKey(namespace, account string) string { return namespace + "/" + account }

// RegisterPod records what the run vouches for about one identity, replacing
// what it vouched for before: a rebuilt workload is the same identity running
// a new build.
func (a *Authority) RegisterPod(pod *Pod) {
	a.mu.Lock()
	defer a.mu.Unlock()
	copied := *pod
	a.pods[podKey(pod.Namespace, pod.ServiceAccount)] = &copied
}

// Pod is the process registered for an identity.
func (a *Authority) Pod(namespace, account string) (Pod, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	pod, ok := a.pods[podKey(namespace, account)]
	if !ok {
		return Pod{}, false
	}
	return *pod, true
}

// Token issues a credential for an identity, naming the instance and, once
// the run knows them, the container and build it registered for it.
func (a *Authority) Token(namespace, account, podName, podUID, audience string) (string, error) {
	subject := native.Subject{Namespace: namespace, Account: account, Instance: podName, InstanceUID: podUID}
	if pod, ok := a.Pod(namespace, account); ok && pod.UID == podUID {
		subject.Container, subject.Build = pod.Container, pod.Digest
		if pod.Digest != "" && pod.Repository != "" {
			subject.DeclaredImage = pod.Repository + "@" + pod.Digest
		}
	}
	return a.issuer.Issue(subject, audience)
}

// Projection is what one workload is handed: the file its credential is in.
type Projection struct {
	// TokenFile is the path of the workload's credential.
	TokenFile string
}

// Workload is one native process to issue a credential for.
type Workload struct {
	Namespace string
	Account   string
	PodName   string
	PodUID    string
	// Audience is the audience its credential is issued for: the host's.
	Audience string
}

// Project writes a workload's credential under dir, owner-only. The
// credential is rewritten by RefreshToken.
func (a *Authority) Project(dir string, workload *Workload) (Projection, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Projection{}, err
	}
	projection := Projection{TokenFile: filepath.Join(dir, "token")}
	return projection, a.RefreshToken(projection.TokenFile, workload)
}

// RefreshToken rewrites a workload's credential, as a kubelet rotates a
// projected token before it lapses, and as soon as the run learns its build.
func (a *Authority) RefreshToken(path string, workload *Workload) error {
	token, err := a.Token(workload.Namespace, workload.Account, workload.PodName, workload.PodUID, workload.Audience)
	if err != nil {
		return err
	}
	return writePrivate(path, []byte(token))
}

// writePrivate writes a file atomically with owner-only permissions, so a
// reader never sees a half-written credential.
func writePrivate(path string, content []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, content, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
