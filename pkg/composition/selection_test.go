package composition

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/codefly-dev/core/composition"
	updatev0 "github.com/codefly-dev/core/generated/go/codefly/update/v0"
	"github.com/codefly-dev/core/moduleupdate"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"gopkg.in/yaml.v3"
)

// selectionRegistry serves signed release declarations over the real GitHub
// resolver boundary. Runtime bytes use a separate TLS origin. No source-fetch
// endpoint exists, so an accidental archive/checkout fallback fails the test.
type selectionRegistry struct {
	api       *httptest.Server
	artifacts *httptest.Server
	key       ed25519.PrivateKey
	mu        sync.Mutex
	releases  map[string]map[string]int
	assets    map[int][]byte
	content   map[string][]byte
	requests  []string
	workspace string
}

func newSelectionRegistry(t *testing.T) *selectionRegistry {
	t.Helper()
	r := &selectionRegistry{key: ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32)), releases: map[string]map[string]int{}, assets: map[int][]byte{}, content: map[string][]byte{}, workspace: t.TempDir()}
	r.artifacts = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.requests = append(r.requests, request.URL.Path)
		data, ok := r.content[request.URL.Path]
		if !ok {
			http.NotFound(w, request)
			return
		}
		_, _ = w.Write(data)
	}))
	r.api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.requests = append(r.requests, request.URL.Path)
		parts := strings.Split(strings.Trim(strings.TrimPrefix(request.URL.Path, "/api/v3"), "/"), "/")
		if len(parts) < 6 || parts[0] != "repos" {
			http.NotFound(w, request)
			return
		}
		id := parts[1] + "/" + parts[2]
		if parts[3] == "releases" && parts[4] == "assets" {
			var assetID int
			_, _ = fmt.Sscanf(parts[5], "%d", &assetID)
			data, ok := r.assets[assetID]
			if !ok {
				http.NotFound(w, request)
				return
			}
			_, _ = w.Write(data)
			return
		}
		tag := parts[len(parts)-1]
		assets, ok := r.releases[id+"@"+tag]
		if !ok {
			http.NotFound(w, request)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if parts[3] == "git" {
			_ = json.NewEncoder(w).Encode(map[string]any{"ref": "refs/tags/" + tag, "object": map[string]string{"type": "commit", "sha": strings.Repeat("a", 40)}})
			return
		}
		var listed []map[string]any
		for name, assetID := range assets {
			listed = append(listed, map[string]any{"id": assetID, "name": name, "size": len(r.assets[assetID])})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "tag_name": tag, "immutable": true, "draft": false, "assets": listed})
	}))
	t.Cleanup(r.api.Close)
	t.Cleanup(r.artifacts.Close)
	t.Setenv("GITHUB_API_URL", r.api.URL+"/")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	return r
}

func selectionManifest(id, version string) *core.PackageManifest {
	return &core.PackageManifest{Kind: core.PackageKind, Schema: core.PackageSchema, ID: id, Version: version,
		MinimumCodeflyVersion: ">=0.1.0", ArtifactRoots: []string{"runtime"}, Contracts: map[string]string{"composition": "^2.0.0"},
		Provides: map[string]string{"interface": "1.0.0"}, RequiredQualifications: &[]string{}}
}

func (r *selectionRegistry) artifact(name string, purpose core.ArtifactPurpose, content []byte) core.ReleaseArtifact {
	r.mu.Lock()
	defer r.mu.Unlock()
	path := "/" + strings.TrimPrefix(contentDigest(content), "sha256:")
	r.content[path] = content
	return core.ReleaseArtifact{Name: name, Purpose: purpose, URI: r.artifacts.URL + path, Digest: contentDigest(content)}
}

func (r *selectionRegistry) publish(t *testing.T, manifest *core.PackageManifest) core.ReleaseSelection {
	t.Helper()
	require.NoError(t, manifest.Validate())
	data, err := yaml.Marshal(manifest)
	require.NoError(t, err)
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "runtime"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, core.PackageManifestFileName), data, 0o600))
	_, digest, err := core.CanonicalArchive(dir)
	require.NoError(t, err)
	provenance, err := json.Marshal(core.Provenance{Schema: core.ProvenanceSchema, Package: manifest.ID, Version: manifest.Version,
		Repository: "https://github.com/" + manifest.ID, Ref: "v" + manifest.Version, Commit: strings.Repeat("a", 40),
		ArtifactMediaType: core.ArtifactMediaType, ArtifactDigest: digest, ManifestDigest: contentDigest(data), SignatureIdentity: "owner"})
	require.NoError(t, err)
	r.mu.Lock()
	defer r.mu.Unlock()
	assets := map[string]int{}
	for name, value := range map[string][]byte{core.PackageManifestFileName: data, "provenance.json": provenance, "provenance.sig": ed25519.Sign(r.key, provenance)} {
		id := len(r.assets) + 1
		r.assets[id] = value
		assets[name] = id
	}
	r.releases[manifest.ID+"@v"+manifest.Version] = assets
	return core.ReleaseSelection{ID: manifest.ID, Version: manifest.Version, Digest: digest}
}

func (r *selectionRegistry) session(t *testing.T, descriptor *core.Descriptor) *SelectionSession {
	t.Helper()
	repositories := map[string]string{}
	signers := map[string]map[string]string{}
	r.mu.Lock()
	for release := range r.releases {
		id, _, _ := strings.Cut(release, "@")
		repositories[id] = "https://github.com/" + id
		signers[id] = map[string]string{"owner": base64.StdEncoding.EncodeToString(r.key.Public().(ed25519.PublicKey))}
	}
	r.mu.Unlock()
	workspace, err := yaml.Marshal(map[string]any{"name": "product", "module-trust": map[string]any{"repositories": repositories, "signers": signers}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(r.workspace, "workspace.codefly.yaml"), workspace, 0o600))
	dir := t.TempDir()
	data, err := yaml.Marshal(descriptor)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, core.DescriptorFileName), data, 0o600))
	identity, err := core.ConfigurationIdentity(bytes.Repeat([]byte{9}, 32), map[string]string{"password": "not-for-output"})
	require.NoError(t, err)
	session, err := NewSelectionSession(r.workspace, dir, identity)
	require.NoError(t, err)
	return session
}

func moduleDefault(name string, release core.ReleaseSelection) core.ProvidedModule {
	return core.ProvidedModule{Name: name, Default: core.ComponentDefault{Release: release, Requirements: map[string]string{"interface": "^1.0.0"}}}
}

func TestTwoProductsSelectIndependentNestedReleases(t *testing.T) {
	r := newSelectionRegistry(t)
	x1 := r.publish(t, selectionManifest("team/x", "1.0.0"))
	x2 := r.publish(t, selectionManifest("team/x", "1.1.0"))
	y1 := r.publish(t, selectionManifest("team/y", "1.0.0"))
	y2 := r.publish(t, selectionManifest("team/y", "1.2.0"))
	foo := selectionManifest("team/foo", "1.0.0")
	foo.Modules = []core.ProvidedModule{moduleDefault("x", x1), moduleDefault("y", y1)}
	root := r.publish(t, foo)
	descriptor := &core.Descriptor{Kind: core.DescriptorKind, Name: "product", Base: core.Base{ID: root.ID, Version: "^1.0.0"}, Modules: core.ModuleInstances{Include: []string{"x", "y"}}}
	a, b := r.session(t, descriptor), r.session(t, descriptor)
	_, err := a.Initialize(t.Context(), &SelectionInputs{Root: root})
	require.NoError(t, err)
	_, err = b.Initialize(t.Context(), &SelectionInputs{Root: root})
	require.NoError(t, err)
	selectedA, err := a.Select(t.Context(), &core.Replacement{Target: "modules/x", Release: x2, Rationale: "consumer A requires X"})
	require.NoError(t, err)
	selectedB, err := b.Select(t.Context(), &core.Replacement{Target: "modules/y", Release: y2, Rationale: "consumer B requires Y"})
	require.NoError(t, err)
	require.Equal(t, root, selectedA.Resolution.Components[0].Selected)
	require.Equal(t, root, selectedB.Resolution.Components[0].Selected)
	require.Equal(t, y1, selectedA.Resolution.Components[2].Selected)
	require.Equal(t, x1, selectedB.Resolution.Components[1].Selected)
	require.NotEqual(t, selectedA.Identity, selectedB.Identity)
	facts, err := a.Upstream(t.Context())
	require.NoError(t, err)
	require.Equal(t, "https://github.com/team/foo", facts[0].OwnerRepository)
	before, err := os.ReadFile(filepath.Join(a.Root, core.DescriptorFileName))
	require.NoError(t, err)
	_, err = a.Select(t.Context(), &core.Replacement{Target: "modules/x", Release: y2, Rationale: "different owner"})
	require.ErrorContains(t, err, "release authority")
	after, err := os.ReadFile(filepath.Join(a.Root, core.DescriptorFileName))
	require.NoError(t, err)
	require.Equal(t, before, after)
	_, err = a.ProposeRemoval(t.Context(), root, "modules/x")
	require.ErrorContains(t, err, "not equivalent")
	foo.Version = "1.1.0"
	foo.Modules[0].Default.Release = x2
	caughtUp := r.publish(t, foo)
	proposal, err := a.ProposeRemoval(t.Context(), caughtUp, "modules/x")
	require.NoError(t, err)
	require.Empty(t, proposal.Descriptor.Replacements)
	after, err = os.ReadFile(filepath.Join(a.Root, core.DescriptorFileName))
	require.NoError(t, err)
	require.Equal(t, before, after, "an equivalence proposal must not mutate selection")
	raw, err := json.Marshal(selectedA)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "not-for-output")
}

func TestLocalCheckoutsPreserveReleasesAndInvalidateIdentity(t *testing.T) {
	r := newSelectionRegistry(t)
	x := selectionManifest("team/x", "1.0.0")
	xRelease := r.publish(t, x)
	rootManifest := selectionManifest("team/foo", "1.0.0")
	rootManifest.Modules = []core.ProvidedModule{moduleDefault("one", xRelease), moduleDefault("two", xRelease)}
	root := r.publish(t, rootManifest)
	session := r.session(t, &core.Descriptor{Kind: core.DescriptorKind, Name: "consumer", Base: core.Base{ID: root.ID, Version: root.Version}, Modules: core.ModuleInstances{Include: []string{"one", "two"}}})
	released, err := session.Initialize(t.Context(), &SelectionInputs{Root: root})
	require.NoError(t, err)
	checkouts := map[string]string{}
	for _, target := range []string{"modules/one", "modules/two"} {
		dir := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(dir, "runtime"), 0o700))
		data, marshalErr := yaml.Marshal(x)
		require.NoError(t, marshalErr)
		require.NoError(t, os.WriteFile(filepath.Join(dir, core.PackageManifestFileName), data, 0o600))
		command := exec.CommandContext(t.Context(), "git", "init", dir)
		output, initErr := command.CombinedOutput()
		require.NoError(t, initErr, string(output))
		checkouts[target] = dir
	}
	local, err := session.Develop(t.Context(), checkouts)
	require.NoError(t, err)
	require.NotEqual(t, released.Identity, local.Identity)
	require.NotNil(t, local.Checkouts["modules/one"].Dirty)
	require.True(t, *local.Checkouts["modules/one"].Dirty)
	require.NoError(t, os.WriteFile(filepath.Join(checkouts["modules/one"], "runtime", "edited"), []byte("private patch"), 0o600))
	edited, err := session.Inspect(t.Context())
	require.NoError(t, err)
	require.NotEqual(t, local.Identity, edited.Identity)
	_, err = session.Admit(t.Context(), &DeploymentFiles{}, core.DeploymentPolicy{}, time.Now())
	require.ErrorContains(t, err, "local development substitutions")
	_, err = session.Develop(t.Context(), map[string]string{"modules/one": ""})
	require.NoError(t, err)
	restored, err := session.Develop(t.Context(), map[string]string{"modules/two": ""})
	require.NoError(t, err)
	require.Equal(t, released.Identity, restored.Identity)
	data, err := os.ReadFile(filepath.Join(checkouts["modules/one"], "runtime", "edited"))
	require.NoError(t, err)
	require.Equal(t, "private patch", string(data))
}

func TestConcurrentSelectionsAndCanceledMutation(t *testing.T) {
	r := newSelectionRegistry(t)
	x1 := r.publish(t, selectionManifest("team/x", "1.0.0"))
	x2 := r.publish(t, selectionManifest("team/x", "1.1.0"))
	rootManifest := selectionManifest("team/foo", "1.0.0")
	rootManifest.Modules = []core.ProvidedModule{moduleDefault("one", x1), moduleDefault("two", x1)}
	root := r.publish(t, rootManifest)
	session := r.session(t, &core.Descriptor{Kind: core.DescriptorKind, Name: "consumer", Base: core.Base{ID: root.ID, Version: root.Version}, Modules: core.ModuleInstances{Include: []string{"one", "two"}}})
	_, err := session.Initialize(t.Context(), &SelectionInputs{Root: root})
	require.NoError(t, err)
	errors := make(chan error, 2)
	for _, target := range []string{"modules/one", "modules/two"} {
		go func() {
			_, err := session.Select(t.Context(), &core.Replacement{Target: target, Release: x2, Rationale: "concurrent product selection"})
			errors <- err
		}()
	}
	require.NoError(t, <-errors)
	require.NoError(t, <-errors)
	inspection, err := session.Inspect(t.Context())
	require.NoError(t, err)
	require.Len(t, inspection.Resolution.Differences, 2)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = session.Select(ctx, &core.Replacement{Target: "modules/one", Release: x1, Rationale: "interrupted"})
	require.ErrorIs(t, err, context.Canceled)
	after, err := session.Inspect(t.Context())
	require.NoError(t, err)
	require.Equal(t, inspection.Identity, after.Identity)
}

func TestAcquisitionAuthenticatesCacheAndNeverCollectsSource(t *testing.T) {
	r := newSelectionRegistry(t)
	manifest := selectionManifest("team/foo", "1.0.0")
	manifest.ReleaseArtifacts = []core.ReleaseArtifact{r.artifact("runtime", core.ArtifactRuntime, []byte("released bytes")), r.artifact("source", core.ArtifactSource, []byte("must not download"))}
	manifest.Services = []core.ProvidedService{{Name: "api", RuntimeArtifacts: []string{"runtime"}}}
	root := r.publish(t, manifest)
	session := r.session(t, &core.Descriptor{Kind: core.DescriptorKind, Name: "product", Base: core.Base{ID: root.ID, Version: root.Version}, Services: core.Services{Include: []string{"api"}}})
	_, err := session.Initialize(t.Context(), &SelectionInputs{Root: root})
	require.NoError(t, err)
	artifacts, err := session.Acquire(t.Context(), r.artifacts.Client())
	require.NoError(t, err)
	require.Len(t, artifacts, 1)
	require.NoError(t, os.WriteFile(artifacts[0].Path, []byte("private patch"), 0o600))
	_, err = session.Acquire(t.Context(), r.artifacts.Client())
	require.NoError(t, err)
	data, err := os.ReadFile(artifacts[0].Path)
	require.NoError(t, err)
	require.Equal(t, "released bytes", string(data))
	r.mu.Lock()
	for _, request := range r.requests {
		require.NotContains(t, request, strings.TrimPrefix(manifest.ReleaseArtifacts[1].Digest, "sha256:"))
	}
	r.content["/"+strings.TrimPrefix(manifest.ReleaseArtifacts[0].Digest, "sha256:")] = []byte("tampered origin")
	r.mu.Unlock()
	require.NoError(t, os.Remove(artifacts[0].Path))
	_, err = session.Acquire(t.Context(), r.artifacts.Client())
	require.ErrorIs(t, err, core.ErrDigestMismatch)
	_, err = os.Stat(artifacts[0].Path)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestCompatibilityAuthenticatesActualConsumerAndReportsUnknown(t *testing.T) {
	r := newSelectionRegistry(t)
	item := &updatev0.ContractItem{Id: "api", Digest: contentDigest([]byte("api contract"))}
	baseline, err := moduleupdate.PrepareSnapshot(&updatev0.ContractSnapshot{SchemaVersion: 1, Module: "team/x", Version: "1.0.0", Complete: true, Items: []*updatev0.ContractItem{item}})
	require.NoError(t, err)
	publish := func(snapshot *updatev0.ContractSnapshot) core.ReleaseSelection {
		data, err := protojson.Marshal(snapshot)
		require.NoError(t, err)
		manifest := selectionManifest(snapshot.Module, snapshot.Version)
		manifest.ReleaseArtifacts = []core.ReleaseArtifact{r.artifact("contracts", core.ArtifactContracts, data)}
		return r.publish(t, manifest)
	}
	x1 := publish(baseline)
	after, err := moduleupdate.PrepareSnapshot(&updatev0.ContractSnapshot{SchemaVersion: 1, Module: "team/x", Version: "1.1.0", Complete: true})
	require.NoError(t, err)
	x2 := publish(after)
	rootManifest := selectionManifest("team/foo", "1.0.0")
	rootManifest.Modules = []core.ProvidedModule{moduleDefault("x", x1)}
	root := r.publish(t, rootManifest)
	session := r.session(t, &core.Descriptor{Kind: core.DescriptorKind, Name: "product", Base: core.Base{ID: root.ID, Version: root.Version}, Modules: core.ModuleInstances{Include: []string{"x"}}})
	_, err = session.Initialize(t.Context(), &SelectionInputs{Root: root, Artifacts: []core.ArtifactRequest{{Target: "modules/x", Name: "contracts"}}})
	require.NoError(t, err)
	selected, err := session.Select(t.Context(), &core.Replacement{Target: "modules/x", Release: x2, Rationale: "candidate"})
	require.NoError(t, err)
	usagePin, err := protojson.Marshal(&updatev0.ConsumerPin{SchemaVersion: 1, Consumer: "product", Module: "team/x", Version: "1.0.0", SnapshotDigest: baseline.Digest, UsageComplete: true, Uses: []*updatev0.ContractUse{{Item: item.Id, Digest: item.Digest}}})
	require.NoError(t, err)
	now := time.Now()
	statement, err := json.Marshal(core.ConsumerUsageStatement{Schema: "codefly/consumer-usage/v1", Instance: "modules/x", CompositionDigest: selected.Identity, Pin: usagePin, Signer: "consumer", ExpiresAt: now.Add(time.Hour)})
	require.NoError(t, err)
	usage := core.SignedConsumerUsage{Statement: statement, Signature: ed25519.Sign(r.key, statement)}
	authority := core.ConsumerUsageAuthority{Consumer: "product", Signers: map[string]ed25519.PublicKey{"consumer": r.key.Public().(ed25519.PublicKey)}}
	inspection, err := session.CheckCompatibility(t.Context(), "modules/x", "contracts", usage, authority, now, r.artifacts.Client())
	require.NoError(t, err)
	require.Contains(t, string(inspection.Result), "VERDICT_BREAKING")
	inspection, err = session.CheckCompatibility(t.Context(), "modules/x", "contracts", usage, authority, now.Add(2*time.Hour), r.artifacts.Client())
	require.NoError(t, err)
	require.Contains(t, string(inspection.Result), "VERDICT_UNDETERMINED")
	inspection, err = session.CheckCompatibility(t.Context(), "modules/x", "contracts", usage, core.ConsumerUsageAuthority{}, now, r.artifacts.Client())
	require.NoError(t, err)
	require.Contains(t, string(inspection.Result), "VERDICT_UNDETERMINED")
}

func TestAdmissionBindsActualRuntimeFilesEvidenceAndTarget(t *testing.T) {
	r := newSelectionRegistry(t)
	manifest := selectionManifest("team/foo", "1.0.0")
	manifest.RequiredQualifications = &[]string{"stateful"}
	manifest.ReleaseArtifacts = []core.ReleaseArtifact{r.artifact("runtime", core.ArtifactRuntime, []byte("owner runtime"))}
	manifest.Services = []core.ProvidedService{{Name: "api", RuntimeArtifacts: []string{"runtime"}}}
	root := r.publish(t, manifest)
	session := r.session(t, &core.Descriptor{Kind: core.DescriptorKind, Name: "product", Base: core.Base{ID: root.ID, Version: root.Version}, Services: core.Services{Include: []string{"api"}}})
	_, err := session.Initialize(t.Context(), &SelectionInputs{Root: root})
	require.NoError(t, err)
	artifacts, err := session.Acquire(t.Context(), r.artifacts.Client())
	require.NoError(t, err)
	files := &DeploymentFiles{Runtime: []RuntimeFile{{Name: "runtime", Path: artifacts[0].Path}}, Bindings: map[string]string{"deployment-target": contentDigest([]byte("cluster identity"))}}
	record, err := session.CheckInputs(t.Context(), files)
	require.NoError(t, err)
	now := time.Now()
	policy := core.DeploymentPolicy{RequiredQualifications: []string{"functional"}, QualificationSigners: map[string]map[string]ed25519.PublicKey{}}
	for _, kind := range []string{"functional", "stateful"} {
		statement, marshalErr := json.Marshal(core.Qualification{Schema: "codefly/deployment-qualification/v1", SelectionIdentity: record.SelectionIdentity,
			RuntimeIdentity: record.RuntimeIdentity, BindingIdentity: record.BindingIdentity, Kind: kind, Signer: "consumer", ExpiresAt: now.Add(time.Hour)})
		require.NoError(t, marshalErr)
		policy.QualificationSigners[kind] = map[string]ed25519.PublicKey{"consumer": r.key.Public().(ed25519.PublicKey)}
		files.Qualifications = append(files.Qualifications, core.SignedQualification{Statement: statement, Signature: ed25519.Sign(r.key, statement)})
	}
	admitted, err := session.Admit(t.Context(), files, policy, now)
	require.NoError(t, err)
	require.Equal(t, record.SelectionIdentity, admitted.Record.SelectionIdentity)
	files.Bindings["deployment-target"] = contentDigest([]byte("different cluster"))
	_, err = session.Admit(t.Context(), files, policy, now)
	require.ErrorContains(t, err, "different inputs")
	files.Bindings["deployment-target"] = contentDigest([]byte("cluster identity"))
	_, err = session.Admit(t.Context(), files, policy, now.Add(2*time.Hour))
	require.ErrorContains(t, err, "expired")
	files.Qualifications = files.Qualifications[:1]
	_, err = session.Admit(t.Context(), files, policy, now)
	require.ErrorContains(t, err, "required stateful qualification")
	require.NoError(t, os.WriteFile(artifacts[0].Path, []byte("private patch"), 0o600))
	_, err = session.Admit(t.Context(), files, policy, now)
	require.ErrorIs(t, err, core.ErrDigestMismatch)
}

func TestNestedAgentReplacementDoesNotChangeModuleOrImplyRuntimeAgent(t *testing.T) {
	r := newSelectionRegistry(t)
	agent := selectionManifest("team/database-agent", "1.0.0")
	agent.ReleaseArtifacts = []core.ReleaseArtifact{r.artifact("lifecycle", core.ArtifactLifecycleAgent, []byte("lifecycle executable"))}
	oldAgent := r.publish(t, agent)
	agent.Version = "1.1.0"
	newAgent := r.publish(t, agent)
	module := selectionManifest("team/storage", "2.0.0")
	module.ReleaseArtifacts = []core.ReleaseArtifact{r.artifact("database", core.ArtifactRuntime, []byte("database runtime"))}
	module.Services = []core.ProvidedService{{Name: "store", Agent: &core.ComponentDefault{Release: oldAgent, Requirements: map[string]string{"interface": "^1.0.0"}}, AgentUsage: "lifecycle", RuntimeArtifacts: []string{"database"}}}
	moduleRelease := r.publish(t, module)
	rootManifest := selectionManifest("team/foo", "1.0.0")
	child := moduleDefault("storage", moduleRelease)
	child.Services.Include = []string{"store"}
	rootManifest.Modules = []core.ProvidedModule{child}
	root := r.publish(t, rootManifest)
	session := r.session(t, &core.Descriptor{Kind: core.DescriptorKind, Name: "product", Base: core.Base{ID: root.ID, Version: root.Version}, Modules: core.ModuleInstances{Include: []string{"storage"}}})
	_, err := session.Initialize(t.Context(), &SelectionInputs{Root: root})
	require.NoError(t, err)
	selected, err := session.Select(t.Context(), &core.Replacement{Target: "modules/storage/services/store/agent", Release: newAgent, Rationale: "qualified lifecycle protocol"})
	require.NoError(t, err)
	require.Equal(t, moduleRelease, selected.Resolution.Components[1].Selected)
	require.Len(t, selected.Resolution.Acquisitions, 2)
	require.Equal(t, core.ArtifactRuntime, selected.Resolution.Acquisitions[0].Artifact.Purpose)
	require.Equal(t, core.ArtifactLifecycleAgent, selected.Resolution.Acquisitions[1].Artifact.Purpose)
	artifacts, err := session.Acquire(t.Context(), r.artifacts.Client())
	require.NoError(t, err)
	files := &DeploymentFiles{Runtime: []RuntimeFile{{Target: "modules/storage", Name: "database", Path: artifacts[0].Path}}, Bindings: map[string]string{"target": contentDigest([]byte("consumer target"))}}
	record, err := session.CheckInputs(t.Context(), files)
	require.NoError(t, err)
	require.Len(t, record.Artifacts, 1, "a lifecycle tool must not be reported as a deployed runtime artifact")
}

func TestLocalCheckoutCannotMaskIncompatibleReleaseSelection(t *testing.T) {
	r := newSelectionRegistry(t)
	module := selectionManifest("team/x", "1.0.0")
	compatible := r.publish(t, module)
	local := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(local, "runtime"), 0o700))
	data, err := yaml.Marshal(module)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(local, core.PackageManifestFileName), data, 0o600))
	module.Version = "2.0.0"
	module.Provides["interface"] = "2.0.0"
	breaking := r.publish(t, module)
	rootManifest := selectionManifest("team/foo", "1.0.0")
	rootManifest.Modules = []core.ProvidedModule{moduleDefault("x", compatible)}
	root := r.publish(t, rootManifest)
	session := r.session(t, &core.Descriptor{Kind: core.DescriptorKind, Name: "product", Base: core.Base{ID: root.ID, Version: root.Version}, Modules: core.ModuleInstances{Include: []string{"x"}}})
	_, err = session.Initialize(t.Context(), &SelectionInputs{Root: root})
	require.NoError(t, err)
	_, err = session.Develop(t.Context(), map[string]string{"modules/x": local})
	require.NoError(t, err)
	_, err = session.Select(t.Context(), &core.Replacement{Target: "modules/x", Release: breaking, Rationale: "candidate masked by local checkout"})
	require.ErrorIs(t, err, core.ErrContract)
	restored, err := session.Develop(t.Context(), map[string]string{"modules/x": ""})
	require.NoError(t, err)
	require.Equal(t, compatible, restored.Resolution.Components[1].Selected)
}
