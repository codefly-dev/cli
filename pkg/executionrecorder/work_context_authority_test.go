package executionrecorder

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/workcontext"
	"github.com/codefly-dev/core/workcontext/conformance"
)

// TestWorkContextAuthorityIsCoresAuthenticator runs core's conformance kit in
// its authenticator mode against THIS package's entrypoint, built field by
// field from the kit's settings: every fixture core mints must be accepted or
// refused here with the same named reason, and a capability carrying a grant
// hop must be refused rather than accepted unchecked. The kit cannot see the
// alias; the compile-time assertion beside Authenticator is the identity half.
func TestWorkContextAuthorityIsCoresAuthenticator(t *testing.T) {
	settings := conformance.New(time.Now())
	authority, err := NewWorkContextAuthority(&WorkContextAuthorityConfig{
		Issuer:                        settings.Issuer,
		Audience:                      settings.Audience,
		Keys:                          StaticKeys(settings.PublicKeys()),
		Revisions:                     settings.Revisions,
		Seals:                         settings.Seals,
		Replay:                        settings.Replay,
		Now:                           settings.Now,
		TrustTheConformanceFixtureKey: settings.TrustTheConformanceFixtureKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	conformance.RunAuthenticator(t, settings, func(ctx context.Context, token string) error {
		_, err := authority.Authenticate(ctx, token)
		return err
	})
}

func TestWorkContextAuthorityRequiresLiveSources(t *testing.T) {
	issuer := newTestIssuer(t)
	for name, config := range map[string]WorkContextAuthorityConfig{
		"no issuer":    {Audience: ExecutionWorkContextAudience, Keys: issuer.keys(), Revisions: issuer.revision, Seals: issuer.seals},
		"no audience":  {Issuer: testIssuerName, Keys: issuer.keys(), Revisions: issuer.revision, Seals: issuer.seals},
		"no keys":      {Issuer: testIssuerName, Audience: ExecutionWorkContextAudience, Revisions: issuer.revision, Seals: issuer.seals},
		"no revisions": {Issuer: testIssuerName, Audience: ExecutionWorkContextAudience, Keys: issuer.keys(), Seals: issuer.seals},
		"no seals":     {Issuer: testIssuerName, Audience: ExecutionWorkContextAudience, Keys: issuer.keys(), Revisions: issuer.revision},
	} {
		if _, err := NewWorkContextAuthority(&config); err == nil || !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
}

// TestWorkContextAuthorityRequiresTheProducerBoundEvidenceScope: the actor's
// effective scopes must grant evidence/append naming THIS producer. A scope
// naming no resource is a wildcard under core's containment rule, and the
// recorder does not take a wildcard for the evidence it appends.
func TestWorkContextAuthorityRequiresTheProducerBoundEvidenceScope(t *testing.T) {
	issuer := newTestIssuer(t)
	authority := issuer.authorityUnderTest(t)
	admission := Admission{ProducerID: "codefly.execution"}

	claims, err := authority.Verify(t.Context(), issuer.mint(t, evidenceScope("codefly.execution")), admission)
	if err != nil {
		t.Fatal(err)
	}
	if claims.GetTenantId() != testTenant || claims.GetOwnerPrincipalId() != testOwner || claims.GetWorkspaceId() != testWorkspace {
		t.Fatalf("claims = %+v", claims)
	}

	for name, scope := range map[string]*basev0.WorkScopeV1{
		"wildcard resource": {ResourceKind: "evidence", Actions: []string{"append"}},
		"other producer":    evidenceScope("other.execution"),
		"other action":      {ResourceKind: "evidence", Actions: []string{"read"}, ResourceIds: []string{"codefly.execution"}},
		"other kind":        {ResourceKind: "record", Actions: []string{"append"}, ResourceIds: []string{"codefly.execution"}},
	} {
		_, err := authority.Verify(t.Context(), issuer.mint(t, scope), admission)
		if err == nil || !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "authorize execution evidence producer") {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
	if _, err := authority.Verify(t.Context(), issuer.mint(t, evidenceScope("codefly.execution")), Admission{}); err == nil {
		t.Fatal("an admission naming no producer was accepted")
	}
}

// TestWorkContextAuthorityRefusesStaleIssuerState: a capability is held to
// the issuer's LIVE state through core's verifier — the authorization
// revision, the installation's revision, the principal's epoch and the
// approved build's incarnation. Each one moving after the mint refuses the
// capability, as revocation, with nothing in this package to skip it.
func TestWorkContextAuthorityRefusesStaleIssuerState(t *testing.T) {
	issuer := newTestIssuer(t)
	authority := issuer.authorityUnderTest(t)
	admission := Admission{ProducerID: "codefly.execution"}
	session := issuer.mint(t, evidenceScope("codefly.execution"))
	if _, err := authority.Verify(t.Context(), session, admission); err != nil {
		t.Fatal(err)
	}

	issuer.revision.set(testRevision + 1)
	if _, err := authority.Verify(t.Context(), session, admission); !errors.Is(err, workcontext.ErrRevoked) || !strings.Contains(err.Error(), "revision") {
		t.Fatalf("superseded authorization revision: err = %v", err)
	}
	issuer.revision.set(testRevision)

	if err := issuer.seals.PutEpoch(testOwner, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := authority.Verify(t.Context(), session, admission); !errors.Is(err, workcontext.ErrRevoked) || !strings.Contains(err.Error(), "epoch") {
		t.Fatalf("advanced principal epoch: err = %v", err)
	}
	// A fresh capability under the new epoch is sound again; the old one stays
	// refused — the epoch only advances.
	if _, err := authority.Verify(t.Context(), issuer.mint(t, evidenceScope("codefly.execution")), admission); err != nil {
		t.Fatal(err)
	}

	current := issuer.mint(t, evidenceScope("codefly.execution"))
	if err := issuer.seals.Put(testOwner, workcontext.Seal{InstallationID: testInstallation, InstallationRevision: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := authority.Verify(t.Context(), current, admission); !errors.Is(err, workcontext.ErrRevoked) || !strings.Contains(err.Error(), "installation revision") {
		t.Fatalf("advanced installation revision: err = %v", err)
	}

	// A workload's capability attests the build it runs; the issuer approving
	// a later incarnation of that principal's build replaces the execution,
	// and the capability sealed to the earlier one is refused.
	const workload = "principal-workload"
	if err := issuer.seals.Put(workload, workcontext.Seal{InstallationID: testInstallation, InstallationRevision: 1}); err != nil {
		t.Fatal(err)
	}
	if err := issuer.seals.PutEpoch(workload, 1); err != nil {
		t.Fatal(err)
	}
	if err := issuer.seals.PutApprovedBuild(workload, testImageDigest, 1); err != nil {
		t.Fatal(err)
	}
	execution := issuer.mintFor(t, workload, "service", workcontext.Execution{ImageDigest: testImageDigest, BuildIncarnation: 1}, evidenceScope("codefly.execution"))
	if _, err := authority.Verify(t.Context(), execution, admission); err != nil {
		t.Fatal(err)
	}
	if err := issuer.seals.PutApprovedBuild(workload, testImageDigest, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := authority.Verify(t.Context(), execution, admission); !errors.Is(err, workcontext.ErrRevoked) || !strings.Contains(err.Error(), "incarnation") {
		t.Fatalf("replaced build incarnation: err = %v", err)
	}
}

// TestNoSecondWorkContextImplementation is the import gate core's README asks
// a consumer to hold: the Work Context has one implementation, in core, and
// this module neither requires the SDK's copy nor imports it anywhere.
func TestNoSecondWorkContextImplementation(t *testing.T) {
	root := moduleRoot(t)
	modFile, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(modFile, []byte("codefly-dev/sdk-go")) {
		t.Fatal("go.mod requires the SDK's Work Context implementation; core/workcontext is the only one")
	}
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if name := entry.Name(); name == ".git" || name == "node_modules" || name == ".lazybox" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if bytes.Contains(content, []byte(`"github.com/codefly-dev/`+`sdk-go/workcontext`)) {
			t.Errorf("%s imports the SDK's Work Context implementation", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
			return directory
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			t.Fatal("no go.mod above the package directory")
		}
		directory = parent
	}
}

const (
	testIssuerName   = "https://accounts.example.test"
	testTenant       = "tenant-codefly"
	testOwner        = "principal-antoine"
	testWorkspace    = "workspace-1"
	testInstallation = "installation-1"
	testRevision     = 7
	testImageDigest  = "sha256:eca6c756839cbd532a6c7cb16fa75263600f3c710738ac267fb3988e03e146aa"
)

// testIssuer mints capabilities with core's Authority — the one mint — over a
// key generated for the test, so the authority under test verifies real
// tokens and nothing minted by this package.
type testIssuer struct {
	authority *workcontext.Authority
	seals     *workcontext.MemorySealSource
	revision  *settableRevision
	public    ed25519.PublicKey
	keyID     string
	now       time.Time
}

func newTestIssuer(t *testing.T) *testIssuer {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	seals := workcontext.NewMemorySealSource()
	if err := seals.Put(testOwner, workcontext.Seal{InstallationID: testInstallation, InstallationRevision: 1}); err != nil {
		t.Fatal(err)
	}
	if err := seals.PutEpoch(testOwner, 1); err != nil {
		t.Fatal(err)
	}
	if err := seals.PutBearsNoExecution(testOwner); err != nil {
		t.Fatal(err)
	}
	issuer := &testIssuer{
		seals: seals, revision: &settableRevision{value: testRevision}, public: public, keyID: "key-1",
		now: time.Date(2026, time.July, 23, 19, 0, 0, 0, time.UTC),
	}
	issuer.authority = &workcontext.Authority{
		Issuer: testIssuerName, KeyID: issuer.keyID, Key: private,
		Revisions: issuer.revision, Seals: seals, Now: func() time.Time { return issuer.now },
	}
	return issuer
}

func (issuer *testIssuer) keys() StaticKeys {
	return StaticKeys{issuer.keyID: issuer.public}
}

func (issuer *testIssuer) authorityUnderTest(t *testing.T) *WorkContextAuthority {
	t.Helper()
	authority, err := NewWorkContextAuthority(&WorkContextAuthorityConfig{
		Issuer: testIssuerName, Audience: ExecutionWorkContextAudience, Keys: issuer.keys(),
		Revisions: issuer.revision, Seals: issuer.seals, Now: func() time.Time { return issuer.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return authority
}

func (issuer *testIssuer) mint(t *testing.T, scopes ...*basev0.WorkScopeV1) string {
	t.Helper()
	return issuer.mintFor(t, testOwner, "human", workcontext.Execution{}, scopes...)
}

func (issuer *testIssuer) mintFor(t *testing.T, owner, kind string, execution workcontext.Execution, scopes ...*basev0.WorkScopeV1) string {
	t.Helper()
	token, _, err := issuer.authority.Start(t.Context(), workcontext.StartInput{
		Execution:          execution,
		TenantID:           testTenant,
		OwnerPrincipalID:   owner,
		OwnerPrincipalKind: kind,
		OrganizationID:     "organization-codefly",
		TaskID:             "task-1",
		Audience:           ExecutionWorkContextAudience,
		AuthorityScopes:    scopes,
		WorkspaceID:        testWorkspace,
		InstallationID:     testInstallation,
		TTL:                time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func evidenceScope(producer string) *basev0.WorkScopeV1 {
	return &basev0.WorkScopeV1{ResourceKind: "evidence", Actions: []string{"append"}, ResourceIds: []string{producer}}
}

type settableRevision struct {
	mu    sync.Mutex
	value uint64
}

func (r *settableRevision) AuthorizationRevision(context.Context, string) (uint64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.value, nil
}

func (r *settableRevision) set(value uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.value = value
}
