package gitops

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/delivery/signing"
	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// fakeSigner uses real keys. Randomized ECDSA signatures make accidental
// re-signing observable; the stable Ed25519 key serves deterministic tests.
type fakeSigner struct {
	signed     int
	nonce      bool
	key        solutionhost.Signer
	signatures [][]byte
	fail       error
}

func (s *fakeSigner) custody() solutionhost.Signer {
	if s.key == nil {
		var key crypto.PrivateKey = ed25519.NewKeyFromSeed(make([]byte, 32))
		if s.nonce {
			var err error
			key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				panic(err)
			}
		}
		var err error
		s.key, err = solutionhost.NewInMemorySigner("dev-0123456789abcdef", key)
		if err != nil {
			panic(err)
		}
	}
	return s.key
}
func (s *fakeSigner) KeyID() string            { return s.custody().KeyID() }
func (s *fakeSigner) Public() crypto.PublicKey { return s.custody().Public() }
func (s *fakeSigner) Sign(message []byte) ([]byte, error) {
	s.signed++
	if s.fail != nil {
		return nil, s.fail
	}
	signature, err := s.custody().Sign(message)
	s.signatures = append(s.signatures, signature)
	return signature, err
}

// deliveryRepository is a clone-shaped repository whose base branch is reachable
// as refs/remotes/origin/main, with target the staged module path inside it.
type deliveryRepository struct {
	repo       string
	target     string
	targetPath string
}

func newDeliveryRepository(t *testing.T) *deliveryRepository {
	t.Helper()
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
	run("init", "-q", "-b", "main")
	run("commit", "-q", "--allow-empty", "-m", "base")
	run("update-ref", "refs/remotes/origin/main", "HEAD")
	targetPath := "deployments/modules/crm"
	return &deliveryRepository{repo: repo, target: filepath.Join(repo, filepath.FromSlash(targetPath)), targetPath: targetPath}
}

// deliver commits whatever is staged under the target as the new base branch,
// the way a merged promotion does.
func (repository *deliveryRepository) deliver(t *testing.T) {
	t.Helper()
	for _, args := range [][]string{{"add", "-A", "--", repository.targetPath}, {"commit", "-q", "--allow-empty", "-m", "promote"}, {"update-ref", "refs/remotes/origin/main", "HEAD"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repository.repo
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
}

// stageRender renders the module tree straight into the repository's target,
// as publish stages it, declaring the named instance — or none, for "" — with
// the fixture's one identity workload.
func (repository *deliveryRepository) stageRender(t *testing.T, instance string) *Inventory {
	t.Helper()
	return repository.stageRenderWith(t, func(options *RenderOptions) {
		if instance == "" {
			options.Instance = nil
			return
		}
		options.Instance.Name, options.Instance.Package = instance, "example/"+instance
		options.Instance.Authority.Principal = instance
	})
}

// stageRenderWith renders the fixture's options, as edit leaves them.
func (repository *deliveryRepository) stageRenderWith(t *testing.T, edit func(*RenderOptions)) *Inventory {
	t.Helper()
	require.NoError(t, os.RemoveAll(repository.target))
	options := solutionRenderOptions(repository.target)
	edit(options)
	result, err := RenderOwnedTree(context.Background(), options, renderWorkload(pinnedDeployment))
	require.NoError(t, err)
	inventory := result.Inventory
	return &inventory
}

// settle settles the staged tree against the repository's base branch.
func (repository *deliveryRepository) settle(inventory *Inventory, opts *deliveryPublishOptions) (*InventoryDelivery, error) {
	return settleManifestDelivery(context.Background(), repository.repo, "main", repository.target, repository.targetPath, "prod", inventory, opts)
}

func settledDocuments(t *testing.T, repository *deliveryRepository) map[string]moduleManifestConfigMap {
	t.Helper()
	directory := filepath.Join(repository.target, filepath.FromSlash(moduleManifestOverlay("prod")))
	entries, err := os.ReadDir(directory)
	require.NoError(t, err)
	documents := map[string]moduleManifestConfigMap{}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		require.NoError(t, err)
		var carrier moduleManifestConfigMap
		require.NoError(t, yaml.Unmarshal(data, &carrier))
		if carrier.Kind == kindConfigMap {
			documents[strings.TrimSuffix(entry.Name(), ".yaml")] = carrier
		}
	}
	return documents
}

// signedManifest reads the manifest a settled carrier signs, as the host reads
// a verified payload: only the canonical encoding is one.
func signedManifest(t *testing.T, repository *deliveryRepository, binding string) *solutionhost.ModuleManifest {
	t.Helper()
	signed, err := solutionhost.ParseSigned([]byte(settledDocuments(t, repository)[binding].Data[solutionhost.SignedFileName]))
	require.NoError(t, err)
	manifest, err := solutionhost.ManifestFromVerified(signed.Document)
	require.NoError(t, err)
	return manifest
}

func documentByID(delivery *InventoryDelivery, id string) InventoryDeliveredDocument {
	for _, document := range delivery.Documents {
		if document.ID == id {
			return document
		}
	}
	return InventoryDeliveredDocument{}
}

func publishOptions(signer signing.Signer) deliveryPublishOptions {
	return deliveryPublishOptions{Signer: signer, Target: testDeliveryTarget(), Domain: "example", Module: "crm"}
}

// TestPublishSettlesGenerationsAgainstTheBaseBranch: a first publish is
// generation 1; an unchanged re-publish keeps it and delivers the carrier it
// delivered before, byte for byte, signing nothing; a change to the manifest's
// canonical content is the next generation — all decided against what the
// base branch delivered.
func TestPublishSettlesGenerationsAgainstTheBaseBranch(t *testing.T) {
	repository := newDeliveryRepository(t)
	signer := &fakeSigner{}
	opts := publishOptions(signer)

	delivery, err := repository.settle(repository.stageRender(t, "crm"), &opts)
	require.NoError(t, err)
	require.True(t, delivery.Signed)
	require.Equal(t, 1, signer.signed)
	require.Equal(t, []InventoryDeliveredDocument{{Kind: deliveredManifestKind, ID: "example.prod.crm", Generation: 1, Digest: delivery.Documents[0].Digest}}, delivery.Documents)
	carrier := settledDocuments(t, repository)["example.prod.crm"]
	signed, err := solutionhost.ParseSigned([]byte(carrier.Data[solutionhost.SignedFileName]))
	require.NoError(t, err)
	manifest, err := solutionhost.ManifestFromVerified(signed.Document)
	require.NoError(t, err, "the carrier holds the canonical bytes verbatim")
	require.Equal(t, uint64(1), manifest.Generation)
	_, err = solutionhost.ManifestFromVerified([]byte(carrier.Data[solutionhost.FileName]))
	require.Error(t, err, "the YAML half is for the repository; the host refuses it as a signed payload")
	kustomization, err := os.ReadFile(filepath.Join(repository.target, filepath.FromSlash(moduleManifestOverlay("prod")), kustomizationFile))
	require.NoError(t, err)
	for _, want := range []string{"example.prod.crm.yaml", "deliver-manifest.yaml", "delivery-service-account.yaml"} {
		require.Contains(t, string(kustomization), want)
	}
	job, err := os.ReadFile(filepath.Join(repository.target, filepath.FromSlash(moduleManifestOverlay("prod")), "deliver-manifest.yaml"))
	require.NoError(t, err)
	for _, want := range []string{"namespace: crm", "serviceAccountName: delivery", "value: " + manifestDeliveryPath, "key: " + solutionhost.SignedFileName} {
		require.Contains(t, string(job), want)
	}
	repository.deliver(t)

	// Unchanged: the same generation, the same carrier, the same Job.
	before := settledDocuments(t, repository)
	delivery, err = repository.settle(repository.stageRender(t, "crm"), &opts)
	require.NoError(t, err)
	require.Equal(t, uint64(1), delivery.Documents[0].Generation)
	require.Equal(t, 1, signer.signed, "an unchanged manifest is not signed again")
	require.True(t, delivery.Signed, "a reused carrier is a signed carrier")
	require.Equal(t, before["example.prod.crm"].Data[solutionhost.SignedFileName], settledDocuments(t, repository)["example.prod.crm"].Data[solutionhost.SignedFileName], "the carrier is byte-identical")
	jobAgain, err := os.ReadFile(filepath.Join(repository.target, filepath.FromSlash(moduleManifestOverlay("prod")), "deliver-manifest.yaml"))
	require.NoError(t, err)
	require.Equal(t, string(job), string(jobAgain), "the same set is the same Job")
	repository.deliver(t)

	// A change to what the manifest says: the next generation, signed anew.
	delivery, err = repository.settle(repository.stageRenderWith(t, func(options *RenderOptions) {
		options.Instance.Endpoints[0].Address = "http://api.crm.svc.cluster.local:9191"
	}), &opts)
	require.NoError(t, err)
	require.Equal(t, uint64(2), delivery.Documents[0].Generation)
	require.Equal(t, 2, signer.signed)
	require.Equal(t, "http://api.crm.svc.cluster.local:9191", signedManifest(t, repository, "example.prod.crm").Endpoints[0].Address)
}

func TestSettledGenerationFollowsCanonicalContent(t *testing.T) {
	prior, err := ModuleManifestOf("example.prod.crm", testHost(), solutionRenderOptions("").Instance,
		[]solutionhost.Workload{{Service: "api", Container: "api"}})
	require.NoError(t, err)
	prior.Generation = 4
	same := *prior
	same.Generation = 1
	generation, err := settledGeneration(prior, &same)
	require.NoError(t, err)
	require.Equal(t, uint64(4), generation, "the same content keeps the delivered generation")
	changed := same
	changed.Endpoints = nil
	generation, err = settledGeneration(prior, &changed)
	require.NoError(t, err)
	require.Equal(t, uint64(5), generation, "other content is the next one")
}

// TestPublishWithdrawsWhatThisModuleStoppedDeclaring is the removal rule: a
// binding this module delivered before and no longer declares is withdrawn —
// its identity alone, removed, at the next generation — carried forward
// verbatim on every later publish, and terminal: presenting the binding again
// is refused at publish with core's own reason.
func TestPublishWithdrawsWhatThisModuleStoppedDeclaring(t *testing.T) {
	repository := newDeliveryRepository(t)
	signer := &fakeSigner{}
	opts := publishOptions(signer)
	_, err := repository.settle(repository.stageRender(t, "billing"), &opts)
	require.NoError(t, err)
	repository.deliver(t)
	delivered := signedManifest(t, repository, "example.prod.billing")

	delivery, err := repository.settle(repository.stageRender(t, "crm"), &opts)
	require.NoError(t, err)
	require.Len(t, delivery.Documents, 2)
	require.Equal(t, InventoryDeliveredDocument{Kind: deliveredManifestKind, ID: "example.prod.billing", Generation: 2, Removed: true, Digest: documentByID(delivery, "example.prod.billing").Digest}, documentByID(delivery, "example.prod.billing"))
	require.Equal(t, uint64(1), documentByID(delivery, "example.prod.crm").Generation)
	withdrawal := signedManifest(t, repository, "example.prod.billing")
	require.Equal(t, &solutionhost.ModuleManifest{
		Schema: delivered.Schema, Binding: delivered.Binding, Alias: delivered.Alias, Kind: delivered.Kind,
		Generation: 2, Removed: true, Host: delivered.Host, Release: delivered.Release,
	}, withdrawal, "a withdrawal carries its identity and nothing else, signed and delivered like any generation")
	carried := settledDocuments(t, repository)["example.prod.billing"].Data[solutionhost.SignedFileName]
	repository.deliver(t)

	// Still gone: carried forward verbatim, at the same generation.
	signed := signer.signed
	delivery, err = repository.settle(repository.stageRender(t, "crm"), &opts)
	require.NoError(t, err)
	require.Equal(t, uint64(2), documentByID(delivery, "example.prod.billing").Generation)
	require.True(t, documentByID(delivery, "example.prod.billing").Removed)
	require.Equal(t, carried, settledDocuments(t, repository)["example.prod.billing"].Data[solutionhost.SignedFileName])
	require.Equal(t, signed, signer.signed, "a withdrawal already delivered is not signed again")
	repository.deliver(t)

	// Presented again under the same binding: refused.
	_, err = repository.settle(repository.stageRender(t, "billing"), &opts)
	require.ErrorIs(t, err, solutionhost.ErrWithdrawn)
	require.Contains(t, err.Error(), "example.prod.billing")
	require.Contains(t, err.Error(), "a new instance needs a new name")
}

func TestWithdrawalOfCarriesOnlyTheIdentity(t *testing.T) {
	live, err := ModuleManifestOf("example.prod.crm", testHost(), solutionRenderOptions("").Instance,
		[]solutionhost.Workload{{Service: "api", Container: "api"}})
	require.NoError(t, err)
	live.Generation = 3
	withdrawal, err := WithdrawalOf(live)
	require.NoError(t, err)
	require.True(t, withdrawal.Removed)
	require.Equal(t, uint64(4), withdrawal.Generation)
	require.Empty(t, withdrawal.Workloads)
	require.Empty(t, withdrawal.Endpoints)
	require.Nil(t, withdrawal.Authority)
	require.Equal(t, live.Binding, withdrawal.Binding)
	again, err := WithdrawalOf(withdrawal)
	require.NoError(t, err)
	require.Equal(t, withdrawal, again, "a withdrawal is carried forward as it is")
	_, err = solutionhost.Admit(live.Host.Coordinate, withdrawal, &solutionhost.ModuleManifest{
		Schema: live.Schema, Binding: live.Binding, Alias: live.Alias, Kind: live.Kind, Generation: 5,
		Host: live.Host, Release: live.Release, Workloads: live.Workloads, Authority: live.Authority,
	})
	require.ErrorIs(t, err, solutionhost.ErrWithdrawn)
}

// TestPublishRefusesACarrierItsOwnCheckRefuses: the publisher verifies each
// carrier right after signing it, as a host would; one the check refuses is
// refused at publish, with nothing written for a Job to deliver.
func TestPublishRefusesACarrierItsOwnCheckRefuses(t *testing.T) {
	repository := newDeliveryRepository(t)
	checked := 0
	opts := publishOptions(&fakeSigner{})
	opts.SelfCheck = func(_ context.Context, envelope, payload []byte) error {
		checked++
		if len(envelope) == 0 || len(payload) == 0 {
			return errors.New("empty")
		}
		return errors.New("the certificate names another workflow")
	}
	_, err := repository.settle(repository.stageRender(t, "crm"), &opts)
	require.ErrorContains(t, err, "module manifest example.prod.crm: the certificate names another workflow")
	require.Equal(t, 1, checked)
	_, statErr := os.Stat(filepath.Join(repository.target, filepath.FromSlash(moduleManifestOverlay("prod")), "deliver-manifest.yaml"))
	require.True(t, os.IsNotExist(statErr), "a refused carrier is never written as a delivery")
}

// TestPublishRefusesUnsignedManifestsOutsideLocalQualification: with no
// signing identity, publish refuses and names the manifests; only a local
// qualification publish may deliver them unsigned, and then writes no Job.
func TestPublishRefusesUnsignedManifestsOutsideLocalQualification(t *testing.T) {
	repository := newDeliveryRepository(t)
	unsigned := signing.FromEnvironment(func(string) (string, bool) { return "", false })
	inventory := repository.stageRender(t, "crm")
	opts := publishOptions(unsigned)
	_, err := repository.settle(inventory, &opts)
	require.ErrorIs(t, err, signing.ErrNoIdentity)
	for _, want := range []string{"example.prod.crm", "VAULT_ADDR", "VAULT_TOKEN", "CODEFLY_RELEASE_TRANSIT_KEY"} {
		require.Contains(t, err.Error(), want)
	}
	opts.AllowUnsigned = true
	delivery, err := repository.settle(inventory, &opts)
	require.NoError(t, err)
	require.False(t, delivery.Signed)
	require.NotContains(t, settledDocuments(t, repository)["example.prod.crm"].Data, solutionhost.SignedFileName)
	kustomization, err := os.ReadFile(filepath.Join(repository.target, filepath.FromSlash(moduleManifestOverlay("prod")), kustomizationFile))
	require.NoError(t, err)
	require.NotContains(t, string(kustomization), "deliver-manifest.yaml", "nothing POSTs a manifest the host refuses")
}

// TestPublishRefusesToWithdrawAnotherDomainsBinding: a withdrawal is authored
// by the domain that delivered the binding.
func TestPublishRefusesToWithdrawAnotherDomainsBinding(t *testing.T) {
	repository := newDeliveryRepository(t)
	opts := publishOptions(&fakeSigner{})
	_, err := repository.settle(repository.stageRender(t, "crm"), &opts)
	require.NoError(t, err)
	repository.deliver(t)
	moved := opts
	moved.Domain = "other"
	_, err = repository.settle(repository.stageRender(t, ""), &moved)
	require.ErrorIs(t, err, solutionhost.ErrWrongDomain)
	require.Contains(t, err.Error(), `module manifest example.prod.crm was delivered under domain "example" and this environment declares "other"`)
}

// TestPublishRefusesABindingThatMovesBetweenDomains: a binding keeps the
// domain it was delivered under; a live generation under another is refused
// at publish with core's reason.
func TestPublishRefusesABindingThatMovesBetweenDomains(t *testing.T) {
	repository := newDeliveryRepository(t)
	opts := publishOptions(&fakeSigner{})
	_, err := repository.settle(repository.stageRender(t, "crm"), &opts)
	require.NoError(t, err)
	repository.deliver(t)
	moved := opts
	moved.Domain = "other"
	inventory := repository.stageRenderWith(t, func(options *RenderOptions) { options.Host.Domain = "other" })
	_, err = repository.settle(inventory, &moved)
	require.ErrorIs(t, err, solutionhost.ErrWrongDomain)
	require.Contains(t, err.Error(), "example.prod.crm")
}

// TestPublishRefusesToWithdrawEverythingWhenTheHostDeclarationIsGone: a
// render that names no host and declares nothing, over a delivery that
// declared bindings, is refused rather than withdrawn.
func TestPublishRefusesToWithdrawEverythingWhenTheHostDeclarationIsGone(t *testing.T) {
	repository := newDeliveryRepository(t)
	opts := publishOptions(&fakeSigner{})
	_, err := repository.settle(repository.stageRender(t, "crm"), &opts)
	require.NoError(t, err)
	repository.deliver(t)
	hostless := repository.stageRenderWith(t, func(options *RenderOptions) { options.Host, options.Instance = nil, nil })
	_, err = repository.settle(hostless, &deliveryPublishOptions{Signer: &fakeSigner{}, Module: "crm"})
	require.ErrorContains(t, err, "example.prod.crm")
	require.ErrorContains(t, err, "withdrawals")
}

// TestPublishHoldsEveryManifestToTheHostDeclaredNow: a manifest rendered
// against another domain, coordinate or trust domain is rendered again before
// it is signed, and one rendered for a host the environment no longer names
// is refused outright.
func TestPublishHoldsEveryManifestToTheHostDeclaredNow(t *testing.T) {
	repository := newDeliveryRepository(t)
	inventory := repository.stageRender(t, "crm")
	for want, opts := range map[string]deliveryPublishOptions{
		`ownership domain "example"`: {Signer: &fakeSigner{}, Target: testDeliveryTarget(), Domain: "other", Module: "crm"},
		"elsewhere/prod/x":           {Signer: &fakeSigner{}, Target: testDeliveryTarget(), Domain: "example", Module: "crm", Coordinate: "elsewhere/prod/x"},
		"names no host now":          {Signer: &fakeSigner{}, Domain: "example", Module: "crm"},
		"trust domain":               {Signer: &fakeSigner{}, Target: testDeliveryTarget(), Domain: "example", Module: "crm", TrustDomain: "other.example"},
	} {
		_, err := repository.settle(inventory, &opts)
		require.ErrorContains(t, err, want)
	}
}

// TestPublishRefusesAReusedCarrierThatSignsOtherBytes: a carrier delivered
// before is reused only once it is held to the manifest it would carry.
func TestPublishRefusesAReusedCarrierThatSignsOtherBytes(t *testing.T) {
	repository := newDeliveryRepository(t)
	opts := publishOptions(&fakeSigner{})
	_, err := repository.settle(repository.stageRender(t, "crm"), &opts)
	require.NoError(t, err)
	path := filepath.Join(repository.target, filepath.FromSlash(moduleManifestOverlay("prod")), "example.prod.crm.yaml")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	// The carrier is JSON inside a YAML scalar; the readable manifest beside
	// it spells the generation as YAML, so neither pattern touches it.
	altered := bytes.Replace(data, []byte(`"generation":1`), []byte(`"generation":7`), 1)
	if bytes.Equal(altered, data) {
		altered = bytes.Replace(data, []byte(`\"generation\":1`), []byte(`\"generation\":7`), 1)
	}
	require.NotEqual(t, data, altered, "the delivered carrier was not found to alter")
	require.NoError(t, os.WriteFile(path, altered, 0o600))
	repository.deliver(t)

	_, err = repository.settle(repository.stageRender(t, "crm"), &opts)
	require.ErrorContains(t, err, "signs other bytes")
}

// TestPublishResignsAReusedCarrierTheReleasePolicyNoLongerAdmits: a carrier
// that still signs its manifest but no longer passes the release policy is
// signed again only when told to (--resign), at the same generation, and the
// publish executing that plan reuses the plan's carrier.
func TestPublishResignsAReusedCarrierTheReleasePolicyNoLongerAdmits(t *testing.T) {
	repository := newDeliveryRepository(t)
	signer := &fakeSigner{nonce: true}
	opts := publishOptions(signer)
	_, err := repository.settle(repository.stageRender(t, "crm"), &opts)
	require.NoError(t, err)
	require.Equal(t, 1, signer.signed)
	repository.deliver(t)

	opts.ReuseCheck = func(_ context.Context, envelope, _ []byte) error {
		if bytes.Contains(envelope, []byte(base64.StdEncoding.EncodeToString(signer.signatures[0]))) {
			return errors.New("the signing identity rotated")
		}
		return nil
	}
	_, err = repository.settle(repository.stageRender(t, "crm"), &opts)
	require.ErrorContains(t, err, "--resign")
	require.Equal(t, 1, signer.signed)
	opts.Resign = true
	delivery, err := repository.settle(repository.stageRender(t, "crm"), &opts)
	require.NoError(t, err)
	require.True(t, delivery.Signed)
	require.Equal(t, 2, signer.signed, "the plan signed the manifest again, once")
	require.Equal(t, uint64(1), documentByID(delivery, "example.prod.crm").Generation, "re-signing is not a new generation")

	executing := opts
	executing.Reuse, executing.Executing, executing.Carriers = opts.Carriers, true, nil
	_, err = repository.settle(repository.stageRender(t, "crm"), &executing)
	require.NoError(t, err)
	require.Equal(t, 2, signer.signed, "the publish reused the plan's carrier")
}

// TestPublishRefusesAPlanWhoseCarrierThePolicyNoLongerAdmits: a plan's
// carrier is held to the release policy as it is at publish; a stale plan is
// refused by name and nothing is signed.
func TestPublishRefusesAPlanWhoseCarrierThePolicyNoLongerAdmits(t *testing.T) {
	repository := newDeliveryRepository(t)
	signer := &fakeSigner{nonce: true}
	opts := publishOptions(signer)
	_, err := repository.settle(repository.stageRender(t, "crm"), &opts)
	require.NoError(t, err)
	require.NotEmpty(t, opts.Carriers, "the plan recorded what it signed")
	executing := opts
	executing.Reuse, executing.Executing, executing.Carriers = opts.Carriers, true, nil
	executing.ReuseCheck = func(context.Context, []byte, []byte) error { return errors.New("the release policy was tightened") }
	_, err = repository.settle(repository.stageRender(t, "crm"), &executing)
	require.ErrorContains(t, err, "plan again")
	require.Equal(t, 1, signer.signed, "an executing publish signs nothing")
}

// TestRollbackResettlesWhatItRestores: a rollback restores an earlier tree
// and settles its manifest anew — the old content at the next generation,
// signed now — and a rollback across a withdrawal is refused; a tree from
// before the module declared anything withdraws what was delivered.
func TestRollbackResettlesWhatItRestores(t *testing.T) {
	ctx := context.Background()
	repository := newDeliveryRepository(t)
	signer := &fakeSigner{}
	opts := publishOptions(signer)
	publication := &deliveryPublication{baseBranch: "main", options: opts}

	delivery, err := repository.settle(repository.stageRender(t, "crm"), &opts)
	require.NoError(t, err)
	require.Equal(t, uint64(1), documentByID(delivery, "example.prod.crm").Generation)
	kept := t.TempDir()
	require.NoError(t, copyTree(repository.target, kept))
	repository.deliver(t)

	delivery, err = repository.settle(repository.stageRenderWith(t, func(options *RenderOptions) {
		options.Instance.Endpoints[0].Address = "http://api.crm.svc.cluster.local:9191"
	}), &opts)
	require.NoError(t, err)
	require.Equal(t, uint64(2), documentByID(delivery, "example.prod.crm").Generation)
	repository.deliver(t)

	require.NoError(t, os.RemoveAll(repository.target))
	require.NoError(t, copyTree(kept, repository.target))
	signed := signer.signed
	restored, err := LoadInventory(repository.target)
	require.NoError(t, err)
	resettled, err := stageAndSettleDelivery(ctx, repository.repo, repository.target, repository.targetPath, kept, &restored, "prod", publication)
	require.NoError(t, err)
	require.Equal(t, uint64(3), documentByID(resettled, "example.prod.crm").Generation)
	require.Greater(t, signer.signed, signed, "the rollback signs what it delivers")
	require.Equal(t, testEndpointAddress, deliveredModuleManifest(t, repository.target, "example.prod.crm").Endpoints[0].Address, "the restored content is what generation 1 declared")
	repository.deliver(t)

	delivery, err = repository.settle(repository.stageRender(t, ""), &opts)
	require.NoError(t, err)
	require.True(t, documentByID(delivery, "example.prod.crm").Removed)
	repository.deliver(t)
	require.NoError(t, os.RemoveAll(repository.target))
	require.NoError(t, copyTree(kept, repository.target))
	restored, err = LoadInventory(repository.target)
	require.NoError(t, err)
	_, err = stageAndSettleDelivery(ctx, repository.repo, repository.target, repository.targetPath, kept, &restored, "prod", publication)
	require.ErrorIs(t, err, solutionhost.ErrWithdrawn)
}

// TestPublishKeepsAnotherEnvironmentsDeliveredHistory: a publish stages its
// own environment's overlay beside every other environment's delivered
// manifests and removes nothing it did not author; and a manifest delivered
// under this environment's name to another host is never published over.
func TestPublishKeepsAnotherEnvironmentsDeliveredHistory(t *testing.T) {
	ctx := context.Background()
	repository := newDeliveryRepository(t)
	opts := publishOptions(&fakeSigner{})
	publication := &deliveryPublication{baseBranch: "main", options: opts}
	_, err := repository.settle(repository.stageRender(t, "crm"), &opts)
	require.NoError(t, err)
	repository.deliver(t)
	_, err = repository.settle(repository.stageRender(t, ""), &opts)
	require.NoError(t, err)
	require.True(t, deliveredModuleManifest(t, repository.target, "example.prod.crm").Removed)
	repository.deliver(t)

	rendered := filepath.Join(t.TempDir(), "tree")
	options := solutionRenderOptions(rendered)
	options.Environment = "staging"
	result, err := RenderOwnedTree(ctx, options, func(ctx context.Context, root string) error {
		if err := renderWorkload(pinnedDeployment)(ctx, root); err != nil {
			return err
		}
		overlays := filepath.Join(root, "services", "api", "overlays")
		return os.Rename(filepath.Join(overlays, "prod"), filepath.Join(overlays, "staging"))
	})
	require.NoError(t, err)
	inventory := result.Inventory
	require.NoError(t, removePublicationRemainder(repository.target, []string{"services"}))
	_, err = stageAndSettleDelivery(ctx, repository.repo, repository.target, repository.targetPath, rendered, &inventory, "staging", publication)
	require.NoError(t, err)
	require.True(t, deliveredModuleManifest(t, repository.target, "example.prod.crm").Removed, "prod's withdrawal survives staging's publish")
	require.False(t, deliveredModuleManifestIn(t, repository.target, "staging", "example.staging.crm").Removed)

	moved := opts
	moved.Coordinate = "elsewhere/prod/x"
	_, err = repository.settle(repository.stageRender(t, "crm"), &moved)
	require.ErrorContains(t, err, "delivered to host")
}

// TestRollbackRestoresOtherEnvironmentsOverlaysAsDelivered: the delivered
// overlays come back from the base branch before anything is settled, so a
// withdrawal delivered since the restored revision survives the restore.
func TestRollbackRestoresOtherEnvironmentsOverlaysAsDelivered(t *testing.T) {
	ctx := context.Background()
	repository := newDeliveryRepository(t)
	opts := publishOptions(&fakeSigner{})
	_, err := repository.settle(repository.stageRender(t, "crm"), &opts)
	require.NoError(t, err)
	repository.deliver(t)
	live, err := gitCommand(ctx, repository.repo, "rev-parse", "refs/remotes/origin/main")
	require.NoError(t, err)
	_, err = repository.settle(repository.stageRender(t, ""), &opts)
	require.NoError(t, err)
	repository.deliver(t)
	require.True(t, deliveredModuleManifest(t, repository.target, "example.prod.crm").Removed)

	require.NoError(t, restoreCloneTree(ctx, repository.repo, repository.targetPath, strings.TrimSpace(live)))
	require.False(t, deliveredModuleManifest(t, repository.target, "example.prod.crm").Removed, "the historical revision holds the live manifest")
	require.NoError(t, restoreDeliveredOverlays(ctx, repository.repo, "main", repository.targetPath))
	require.True(t, deliveredModuleManifest(t, repository.target, "example.prod.crm").Removed, "the withdrawal the base branch delivered survives the restore")
}

func TestReleasePublishRequiresTheWorkflowIdentityToDeliver(t *testing.T) {
	check := signing.SelfCheck(func(context.Context, []byte, []byte) error { return nil })
	for name, options := range map[string]deliveryPublishOptions{
		"local":              {AllowUnsigned: true},
		"supplied signer":    {Signer: &fakeSigner{}},
		"admitted key":       {Signer: &fakeSigner{}, SelfCheck: check, ReuseCheck: check},
		"self-check only":    {SelfCheck: check},
		"reuse policy only":  {ReuseCheck: check},
		"no identity at all": {},
	} {
		err := (&deliveryPublication{options: options}).requireReleaseIdentity()
		refused := err != nil
		if want := name == "supplied signer" || name == "self-check only" || name == "reuse policy only" || name == "no identity at all"; refused != want {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
}

// TestOnlyHostedDeliveryNeedsTheWorkflowIdentity: addressing the host is
// where the release identity is required, and only an environment that
// declares a host is addressed — a hostless publication or rollback goes
// through the same preparation and must not be refused for an identity it
// has no use for.
func TestOnlyHostedDeliveryNeedsTheWorkflowIdentity(t *testing.T) {
	ctx := context.Background()
	hostless := &deliveryPublication{baseBranch: "main"}
	require.NoError(t, hostless.addressHost(ctx, nil, &environments.Environment{Name: "production"}))
	require.Nil(t, hostless.options.Target)

	host := &environments.EnvironmentHost{
		Coordinate: "example/prod/region-a", Component: "platform-host", Domain: "example", Audience: "accounts",
		TrustDomain: "cluster.example", Delivery: "payments/host/rest",
	}
	// A hosted environment stating no release policy cannot be released to.
	hosted := &deliveryPublication{baseBranch: "main"}
	err := hosted.addressHost(ctx, nil, &environments.Environment{Name: "production", Host: host})
	require.Error(t, err)
	require.Contains(t, err.Error(), "host.release")
	// With the policy, the workflow identity is required.
	host.Release = &environments.HostReleasePolicy{KeyID: "release-v1"}
	for _, name := range []string{"VAULT_ADDR", "VAULT_TOKEN", "CODEFLY_RELEASE_TRANSIT_KEY", "CODEFLY_RELEASE_KMS_KEY"} {
		t.Setenv(name, "")
		require.NoError(t, os.Unsetenv(name))
	}
	hosted = &deliveryPublication{baseBranch: "main"}
	err = hosted.addressHost(ctx, nil, &environments.Environment{Name: "production", Host: host})
	require.Error(t, err)
	require.Contains(t, err.Error(), "VAULT_ADDR")
}
