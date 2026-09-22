package composition

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/codefly-dev/cli/pkg/internal/mutationauthority"
	core "github.com/codefly-dev/core/composition"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"gopkg.in/yaml.v3"
)

func publicationFixture(t *testing.T) (*SelectionSession, *StageOptions, *StagedBuild, *DeploymentFiles, *BuildPublicationOptions) {
	t.Helper()
	session, stage, registry := buildStageFixture(t)
	workspacePath := filepath.Join(registry.workspace, resources.WorkspaceConfigurationName)
	data, err := os.ReadFile(workspacePath)
	require.NoError(t, err)
	var workspace rawWorkspaceModuleTrustProbe
	require.NoError(t, yaml.Unmarshal(data, &workspace))
	workspace.ModuleTrust.BuildSigners = map[string]map[string]string{"team/module": {"packager": base64.StdEncoding.EncodeToString(registry.key.Public().(ed25519.PublicKey))}}
	data, err = yaml.Marshal(map[string]any{"name": "product", "module-trust": workspace.ModuleTrust})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(workspacePath, data, 0o600))
	session, err = NewSelectionSession(registry.workspace, session.Root, session.ConfigurationIdentity)
	require.NoError(t, err)
	built, err := session.StageBuild(t.Context(), stage)
	require.NoError(t, err)
	directory, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	options := &BuildPublicationOptions{ExpectedSelection: built.SelectionIdentity, ExpectedBuild: built.Identity, Repository: "invalid.example.test/team/app", Destination: filepath.Join(directory, "published.json"), Signers: map[string]BuildSigningKey{"team/module": {Signer: "packager", Key: registry.key}}}
	return session, stage, built, &DeploymentFiles{Bindings: map[string]string{"target": contentDigest([]byte("qualification-target"))}}, options
}

func TestBuildPublicationRefusesUnapprovedOrChangedInputsBeforeUpload(t *testing.T) {
	session, _, built, files, options := publicationFixture(t)
	_, err := session.PublishBuild(t.Context(), built, files, options, mutationauthority.PreparedPermit{})
	require.ErrorContains(t, err, "prepared authority")
	permit := mutationauthority.NewPreparedPermit()
	for _, change := range []struct {
		name string
		edit func(*BuildPublicationOptions, *StagedBuild, *DeploymentFiles)
		want string
	}{
		{"selection", func(o *BuildPublicationOptions, _ *StagedBuild, _ *DeploymentFiles) {
			o.ExpectedSelection = contentDigest(nil)
		}, "inspected selection"},
		{"key", func(o *BuildPublicationOptions, _ *StagedBuild, _ *DeploymentFiles) {
			o.Signers = map[string]BuildSigningKey{"team/module": {Signer: "packager", Key: ed25519.NewKeyFromSeed(bytes.Repeat([]byte{5}, 32))}}
		}, "package-scoped"},
		{"missing execution", func(_ *BuildPublicationOptions, b *StagedBuild, _ *DeploymentFiles) { b.Executions = b.Executions[:1] }, "invocation digest"},
		{"duplicate execution", func(_ *BuildPublicationOptions, b *StagedBuild, _ *DeploymentFiles) {
			b.Executions = []ExecutionFiles{b.Executions[0], b.Executions[0]}
		}, "invocation digest"},
		{"old qualification", func(_ *BuildPublicationOptions, _ *StagedBuild, f *DeploymentFiles) {
			f.Qualifications = []core.SignedQualification{{Statement: []byte("old")}}
		}, "fresh inputs"},
		{"mutable tag", func(o *BuildPublicationOptions, _ *StagedBuild, _ *DeploymentFiles) { o.Repository += ":latest" }, "without a tag"},
		{"registry without repository", func(o *BuildPublicationOptions, _ *StagedBuild, _ *DeploymentFiles) {
			o.Repository = "registry.example.test"
		}, "registry/repository"},
		{"missing target bindings", func(_ *BuildPublicationOptions, _ *StagedBuild, f *DeploymentFiles) { f.Bindings = nil }, "bindings"},
	} {
		t.Run(change.name, func(t *testing.T) {
			candidate, staged, inputs := *options, *built, *files
			change.edit(&candidate, &staged, &inputs)
			result, publishErr := session.PublishBuild(t.Context(), &staged, &inputs, &candidate, permit)
			require.ErrorContains(t, publishErr, change.want)
			require.NotErrorIs(t, publishErr, ErrBuildPublicationUncertain)
			require.Nil(t, result)
			entries, readErr := os.ReadDir(filepath.Dir(options.Destination))
			require.NoError(t, readErr)
			require.Empty(t, entries)
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = session.PublishBuild(ctx, built, files, options, permit)
	require.ErrorIs(t, err, context.Canceled)
}

func TestPublishedInputPathMustMatchExactlyOnce(t *testing.T) {
	for _, paths := range [][]string{{"other"}, {"private", "private"}} {
		inputs := make([]RuntimeFile, len(paths))
		for i, path := range paths {
			inputs[i].Path = path
		}
		require.Error(t, replacePublishedInputPath(inputs, "private", "cache"))
		for i, path := range paths {
			require.Equal(t, path, inputs[i].Path, "failed rewrite cannot leave a partially changed list")
		}
	}
	inputs := []RuntimeFile{{Path: "other"}, {Path: "private"}}
	require.NoError(t, replacePublishedInputPath(inputs, "private", "cache"))
	require.Equal(t, []RuntimeFile{{Path: "other"}, {Path: "cache"}}, inputs)
}

func TestBuildPublicationUploadsUseRetainedSnapshotHandle(t *testing.T) {
	if os.Getenv("CODEFLY_COMPOSITION_OCI_QUALIFY") != "1" {
		t.Skip("set CODEFLY_COMPOSITION_OCI_QUALIFY=1 for a real snapshot upload")
	}
	_, _, repository, _, collect := startArtifactRegistry(t)
	directory := filepath.Join(t.TempDir(), "private")
	require.NoError(t, os.Mkdir(directory, 0o700))
	root, err := os.OpenRoot(directory)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, root.Close()) })
	data := []byte("verified private output")
	require.NoError(t, root.WriteFile("0000", data, 0o600))
	output := derivedPublication{artifact: core.ReleaseArtifact{Digest: contentDigest(data), MediaType: "application/octet-stream"}, path: filepath.Join(directory, "0000"), directory: root, name: "0000"}
	require.NoError(t, os.Rename(directory, directory+"-retained"))
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "0000"), []byte("unrelated replacement"), 0o600))
	require.NoError(t, os.Symlink(outside, directory))
	require.NoError(t, pushDerivedOutput(t.Context(), repository, &output))
	_, err = retainPublishedOutputs(t.Context(), repository, []derivedPublication{output}, &StagedBuild{Identity: contentDigest(nil), SelectionIdentity: contentDigest(nil)})
	require.NoError(t, err)
	collect()
	_, reader, err := repository.Blobs().FetchReference(t.Context(), digest.FromBytes(data).String())
	require.NoError(t, err)
	actual, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	require.Equal(t, data, actual)
	untouched, err := os.ReadFile(filepath.Join(outside, "0000"))
	require.NoError(t, err)
	require.Equal(t, "unrelated replacement", string(untouched))
}

func TestBuildPublicationRealRegistryAndSignedRenderHandoff(t *testing.T) {
	if os.Getenv("CODEFLY_COMPOSITION_OCI_QUALIFY") != "1" {
		t.Skip("set CODEFLY_COMPOSITION_OCI_QUALIFY=1 for real derived publication")
	}
	session, stage, built, files, options := publicationFixture(t)
	endpoint, client, _, _, collect := startArtifactRegistry(t)
	options.Repository, options.HTTPClient = endpoint+"/team/app", client
	permit := mutationauthority.NewPreparedPermit()
	result, err := session.PublishBuild(t.Context(), built, files, options, permit)
	require.NoError(t, err)
	require.Len(t, result.Inputs.Derived, 2)
	require.Len(t, result.Inputs.Runtime, 2)
	collect()
	for _, signed := range result.Inputs.Derived {
		var statement core.DerivedOutput
		require.NoError(t, json.Unmarshal(signed.Statement, &statement))
		require.Equal(t, "oci://"+options.Repository+"@"+statement.Digest, statement.URI)
		require.True(t, ed25519.Verify(options.Signers["team/module"].Key.Public().(ed25519.PublicKey), signed.Statement, signed.Signature))
		reader, fetchErr := fetchArtifact(t.Context(), client, &core.ReleaseArtifact{URI: statement.URI, Digest: statement.Digest, MediaType: "application/octet-stream"})
		require.NoError(t, fetchErr, "ordinary registry GC must retain published bytes")
		remoteBytes, readErr := io.ReadAll(reader)
		require.NoError(t, readErr)
		require.NoError(t, reader.Close())
		require.Equal(t, statement.Digest, contentDigest(remoteBytes))
	}
	data, err := os.ReadFile(options.Destination)
	require.NoError(t, err)
	var retained DeploymentFiles
	require.NoError(t, json.Unmarshal(data, &retained))
	require.Equal(t, result.Inputs, retained)
	require.NoError(t, os.RemoveAll(built.Directory))
	checked, err := session.CheckInputs(t.Context(), &retained)
	require.NoError(t, err)
	require.Equal(t, built.SelectionIdentity, checked.SelectionIdentity)
	rendered, err := session.StageRender(t.Context(), &retained, stage)
	require.NoError(t, err)
	require.NotEmpty(t, rendered.Record.ExecutionIdentity)
	_, err = session.PublishBuild(t.Context(), built, files, options, permit)
	require.ErrorContains(t, err, "already exist")
	after, err := os.ReadFile(options.Destination)
	require.NoError(t, err)
	require.Equal(t, data, after)
}

func TestBuildPublicationRejectsChangedBytesEvenWithMatchingUpdatedReceipt(t *testing.T) {
	session, _, built, files, options := publicationFixture(t)
	var receipt basev0.ArtifactExecutionReceipt
	require.NoError(t, protojson.Unmarshal(built.Executions[0].Receipt, &receipt))
	data := []byte("private bytes unrelated to the observed source build")
	require.NoError(t, os.WriteFile(filepath.Join(built.Executions[0].Directory, receipt.Outputs[0].Path), data, 0o600))
	receipt.Outputs[0].Digest = contentDigest(data)
	var err error
	built.Executions[0].Receipt, err = protojson.Marshal(&receipt)
	require.NoError(t, err)
	built.Identity, err = buildEvidenceIdentity(built)
	require.NoError(t, err)
	result, err := session.PublishBuild(t.Context(), built, files, options, mutationauthority.NewPreparedPermit())
	require.ErrorContains(t, err, "independently retained invocation digest")
	require.NotErrorIs(t, err, ErrBuildPublicationUncertain)
	require.Nil(t, result)
	entries, err := os.ReadDir(filepath.Dir(options.Destination))
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestBuildPublicationCommandUsesPreparedAuthorityWithRealRegistry(t *testing.T) {
	if os.Getenv("CODEFLY_COMPOSITION_OCI_QUALIFY") != "1" {
		t.Skip("set CODEFLY_COMPOSITION_OCI_QUALIFY=1 for real publication command qualification")
	}
	session, stage, built, files, options := publicationFixture(t)
	endpoint, _, _, certificate, _ := startArtifactRegistry(t)
	dir := t.TempDir()
	write := func(name string, value any) string {
		data, err := json.Marshal(value)
		require.NoError(t, err)
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, data, 0o600))
		return path
	}
	identityKey := filepath.Join(dir, "identity-key")
	signingKey := filepath.Join(dir, "signing-key")
	require.NoError(t, os.WriteFile(identityKey, stage.IdentityKey, 0o600))
	require.NoError(t, os.WriteFile(signingKey, options.Signers["team/module"].Key, 0o600))
	arguments := []string{"run", "../../cmd/codefly", "composition", "--workspace", filepath.Dir(session.trustPath), "--product", session.Root,
		"--identity-key", identityKey, "--render-requests", write("render.json", stage.Requests), "--build-requests", write("build.json", stage.BuildRequests),
		"publish-build", built.EvidenceFile, write("inputs.json", files), write("signers.json", map[string]map[string]string{"team/module": {"signer": "packager", "keyFile": signingKey}}),
		"--expected-selection", built.SelectionIdentity, "--expected-build", built.Identity, "--repository", endpoint + "/team/app", "--output", options.Destination}
	command := exec.CommandContext(t.Context(), "go", arguments...)
	command.Env = append(os.Environ(), "GOWORK=off", "SSL_CERT_FILE="+certificate)
	// stdout carries the result and nothing else; narration is read separately
	// so an interleaved line fails as itself instead of as invalid JSON.
	var narration bytes.Buffer
	command.Stderr = &narration
	output, err := command.Output()
	require.NoError(t, err, "%s\n%s", output, narration.String())
	var published PublishedBuild
	require.NoError(t, json.Unmarshal(output, &published), "%s", output)
	require.Len(t, published.Inputs.Derived, 2)
	require.FileExists(t, options.Destination)
	secret := base64.StdEncoding.EncodeToString(options.Signers["team/module"].Key)
	require.NotContains(t, string(output), secret)
	require.NotContains(t, narration.String(), secret)
}

// Observe actual successful registry reads without substituting any response.
// This places cancellation/competing publication after a real remote write.
type publicationReadObserver struct {
	http.RoundTripper
	onRead func()
	once   sync.Once
}

func (observer *publicationReadObserver) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := observer.RoundTripper.RoundTrip(request)
	if err == nil && response.StatusCode == http.StatusOK && request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/blobs/sha256:") {
		observer.once.Do(observer.onRead)
	}
	return response, err
}

func TestBuildPublicationKeepsRemoteBytesAndReportsPostUploadUncertainty(t *testing.T) {
	if os.Getenv("CODEFLY_COMPOSITION_OCI_QUALIFY") != "1" {
		t.Skip("set CODEFLY_COMPOSITION_OCI_QUALIFY=1 for real publication interruption tests")
	}
	session, _, built, files, options := publicationFixture(t)
	endpoint, client, repository, _, _ := startArtifactRegistry(t)
	options.Repository = endpoint + "/team/app"
	var receipt basev0.ArtifactExecutionReceipt
	require.NoError(t, protojson.Unmarshal(built.Executions[0].Receipt, &receipt))
	for _, failure := range []string{"cancel", "occupied"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			candidate := *options
			candidate.Destination = filepath.Join(filepath.Dir(options.Destination), failure+".json")
			observed := *client
			observed.Transport = &publicationReadObserver{RoundTripper: client.Transport, onRead: func() {
				if failure == "cancel" {
					cancel()
				} else {
					require.NoError(t, os.WriteFile(candidate.Destination, []byte("independent record"), 0o600))
				}
			}}
			candidate.HTTPClient = &observed
			result, err := session.PublishBuild(ctx, built, files, &candidate, mutationauthority.NewPreparedPermit())
			require.ErrorIs(t, err, ErrBuildPublicationUncertain)
			require.Nil(t, result)
			_, reader, err := repository.Blobs().FetchReference(t.Context(), receipt.Outputs[0].Digest)
			require.NoError(t, err)
			data, err := io.ReadAll(reader)
			require.NoError(t, err)
			require.NoError(t, reader.Close())
			require.Equal(t, receipt.Outputs[0].Digest, contentDigest(data), "interruption must not delete shared remote bytes")
			retained, err := os.ReadFile(candidate.Destination)
			if failure == "cancel" {
				require.ErrorIs(t, err, os.ErrNotExist)
			} else {
				require.NoError(t, err)
				require.Equal(t, "independent record", string(retained))
			}
		})
	}
	options.HTTPClient = client
	_, err := session.PublishBuild(t.Context(), built, files, options, mutationauthority.NewPreparedPermit())
	require.NoError(t, err, "fresh authorized retry can reuse exact digest-addressed bytes without replacing a record")
	entries, err := os.ReadDir(filepath.Dir(options.Destination))
	require.NoError(t, err)
	require.Len(t, entries, 2, "private temporary copies must be cleaned, preserving both completed records")
}
