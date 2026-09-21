package composition

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"

	"github.com/codefly-dev/cli/pkg/internal/mutationauthority"
	core "github.com/codefly-dev/core/composition"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
)

var ErrBuildPublicationUncertain = errors.New("derived publication may have retained remote bytes; inspect the destination before retrying")

type BuildSigningKey struct {
	Signer string
	Key    ed25519.PrivateKey
}

type BuildPublicationOptions struct {
	ExpectedSelection string
	ExpectedBuild     string
	Repository        string
	Destination       string
	Signers           map[string]BuildSigningKey
	HTTPClient        *http.Client
}

type PublishedBuild struct {
	SelectionIdentity  string          `json:"selectionIdentity"`
	RetentionReference string          `json:"retentionReference"`
	Inputs             DeploymentFiles `json:"inputs"`
}

// BuildPublicationMutation is local control-plane transport. Signing keys are
// held only for this prepared operation; it is not a remote authorization API.
type BuildPublicationMutation struct {
	Workspace, Product, ConfigurationIdentity string
	Staged                                    StagedBuild
	Inputs                                    DeploymentFiles
	Options                                   BuildPublicationOptions
}

func (mutation *BuildPublicationMutation) Clone() (*BuildPublicationMutation, error) {
	if mutation == nil {
		return nil, errors.New("build publication mutation is required")
	}
	cloned := *mutation
	data, err := json.Marshal(struct {
		Staged StagedBuild
		Inputs DeploymentFiles
	}{mutation.Staged, mutation.Inputs})
	if err != nil {
		return nil, err
	}
	var evidence struct {
		Staged StagedBuild
		Inputs DeploymentFiles
	}
	if err = json.Unmarshal(data, &evidence); err != nil {
		return nil, err
	}
	cloned.Staged, cloned.Inputs = evidence.Staged, evidence.Inputs
	cloned.Options.Signers = maps.Clone(mutation.Options.Signers)
	for owner, signer := range cloned.Options.Signers {
		signer.Key = slices.Clone(signer.Key)
		cloned.Options.Signers[owner] = signer
	}
	return &cloned, nil
}

func ExecuteBuildPublication(ctx context.Context, mutation *BuildPublicationMutation, permit mutationauthority.PreparedPermit) (*PublishedBuild, error) {
	if err := permit.Validate(); err != nil {
		return nil, err
	}
	if mutation == nil {
		return nil, errors.New("build publication mutation is required")
	}
	session, err := NewSelectionSession(mutation.Workspace, mutation.Product, mutation.ConfigurationIdentity)
	if err != nil {
		return nil, err
	}
	return session.PublishBuild(ctx, &mutation.Staged, &mutation.Inputs, &mutation.Options, permit)
}

type derivedPublication struct {
	artifact  core.ReleaseArtifact
	path      string
	directory *os.Root
	name      string
}

// PublishBuild publishes digest-addressed bytes with a content-addressed OCI
// retention reference, not module releases, qualifications or approvals.
func (session *SelectionSession) PublishBuild(ctx context.Context, staged *StagedBuild, files *DeploymentFiles, options *BuildPublicationOptions, permit mutationauthority.PreparedPermit) (*PublishedBuild, error) {
	if err := permit.Validate(); err != nil {
		return nil, err
	}
	if err := validateBuildPublication(staged, files, options); err != nil {
		return nil, err
	}
	repository, repositoryErr := publicationRepository(options.Repository, options.HTTPClient)
	if repositoryErr != nil {
		return nil, repositoryErr
	}
	var published *PublishedBuild
	runErr := session.withResolved(ctx, func(snapshot *selectionSnapshot, resolved *core.ResolvedComposition) (resultErr error) {
		attempted := false
		defer func() {
			if resultErr != nil && attempted {
				resultErr = errors.Join(ErrBuildPublicationUncertain, resultErr)
			}
		}()
		if resolved.Identity() != options.ExpectedSelection || staged.SelectionIdentity != options.ExpectedSelection {
			return errors.New("build publication differs from the independently inspected selection")
		}
		evidenceIdentity, evidenceErr := buildEvidenceIdentity(staged)
		if evidenceErr != nil || evidenceIdentity != options.ExpectedBuild || staged.Identity != options.ExpectedBuild {
			return errors.New("build evidence differs from the independently retained invocation digest")
		}
		if err := session.checkStagingParent(filepath.Dir(options.Destination), snapshot.local); err != nil {
			return err
		}
		if _, err := os.Lstat(options.Destination); !os.IsNotExist(err) {
			return errors.New("build publication destination must not already exist")
		}
		// Snapshot every validated output before issuing a signature or making a
		// remote request. Concurrent staging-path replacement cannot change a push.
		directory, err := os.MkdirTemp(filepath.Dir(options.Destination), ".codefly-publish-build-")
		if err != nil {
			return err
		}
		defer func() { resultErr = errors.Join(resultErr, os.RemoveAll(directory)) }()
		if err = validateStagingDirectory(directory); err != nil {
			return err
		}
		root, err := os.OpenRoot(directory)
		if err != nil {
			return err
		}
		defer func() { resultErr = errors.Join(resultErr, root.Close()) }()
		candidate, outputs, err := session.prepareBuildPublication(ctx, resolved, staged, files, options, root)
		if err != nil {
			return err
		}
		if err = session.checkPublicationInputs(ctx, snapshot, resolved, &candidate.Inputs); err != nil {
			return err
		}
		for i := range outputs {
			if err = errors.Join(ctx.Err(), session.unchanged(ctx, snapshot)); err != nil {
				return err
			}
			attempted = true
			path, publishErr := session.publishDerivedOutput(ctx, repository, options.HTTPClient, &outputs[i])
			if publishErr != nil {
				return publishErr
			}
			if err = replacePublishedInputPath(candidate.Inputs.Runtime, outputs[i].path, path); err != nil {
				return err
			}
		}
		retention, retentionErr := retainPublishedOutputs(ctx, repository, outputs, staged)
		if retentionErr != nil {
			return retentionErr
		}
		candidate.RetentionReference = "oci://" + options.Repository + ":" + retention
		if err = session.checkPublicationInputs(ctx, snapshot, resolved, &candidate.Inputs); err != nil {
			return err
		}
		data, err := json.MarshalIndent(candidate.Inputs, "", "  ")
		if err != nil {
			return err
		}
		if err = publishAdmissionRecord(ctx, options.Destination, data); err != nil {
			return err
		}
		// Assign only after the complete, independently checked record is durable.
		published = candidate
		return nil
	})
	if runErr != nil {
		return nil, runErr
	}
	return published, nil
}

func replacePublishedInputPath(inputs []RuntimeFile, from, to string) error {
	index := -1
	for i := range inputs {
		if inputs[i].Path != from {
			continue
		}
		if index != -1 {
			return errors.New("published output has ambiguous runtime input paths")
		}
		index = i
	}
	if index == -1 {
		return errors.New("published output has no runtime input path")
	}
	inputs[index].Path = to
	return nil
}

// A digest-only blob upload has no GC root. The OCI manifest retains the exact
// output bytes; its digest-derived tag also survives deletion of untagged
// manifests. Consumers never resolve this tag to choose their runtime bytes.
func retainPublishedOutputs(ctx context.Context, repository *remote.Repository, outputs []derivedPublication, staged *StagedBuild) (string, error) {
	empty := []byte("{}")
	config := ocispec.Descriptor{MediaType: ocispec.MediaTypeEmptyJSON, Digest: digest.FromBytes(empty), Size: int64(len(empty))}
	manifest := ocispec.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageManifest, ArtifactType: "application/vnd.codefly.build-outputs.v1", Config: config,
		Annotations: map[string]string{"dev.codefly.selection": staged.SelectionIdentity, "dev.codefly.build": staged.Identity}}
	for _, output := range outputs {
		file, err := openInputFile(ctx, output.directory, output.name)
		if err != nil {
			return "", err
		}
		info, err := file.Stat()
		if err = errors.Join(err, file.Close()); err != nil {
			return "", err
		}
		manifest.Layers = append(manifest.Layers, ocispec.Descriptor{MediaType: output.artifact.MediaType, Digest: digest.Digest(output.artifact.Digest), Size: info.Size()})
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return "", err
	}
	descriptor := ocispec.Descriptor{MediaType: manifest.MediaType, Digest: digest.FromBytes(encoded), Size: int64(len(encoded))}
	reference := "sha256-" + descriptor.Digest.Encoded()
	if err = repository.Blobs().Push(ctx, config, bytes.NewReader(empty)); err != nil {
		return "", errors.Join(ctx.Err(), errors.New("publication retention config upload failed"))
	}
	if err = repository.PushReference(ctx, descriptor, bytes.NewReader(encoded), reference); err != nil {
		return "", errors.Join(ctx.Err(), errors.New("publication retention manifest upload failed"))
	}
	actual, reader, err := repository.FetchReference(ctx, reference)
	if err != nil {
		return "", errors.Join(ctx.Err(), errors.New("publication retention manifest readback failed"))
	}
	data, readErr := io.ReadAll(io.LimitReader(reader, int64(len(encoded))+1))
	err = errors.Join(readErr, reader.Close())
	if err != nil || actual.Digest != descriptor.Digest || !bytes.Equal(data, encoded) {
		return "", errors.Join(err, errors.New("publication retention manifest readback differs from published bytes"))
	}
	return reference, nil
}

func validateBuildPublication(staged *StagedBuild, files *DeploymentFiles, options *BuildPublicationOptions) error {
	if options == nil || staged == nil || files == nil || options.ExpectedSelection == "" || options.ExpectedBuild == "" || !filepath.IsAbs(options.Destination) {
		return errors.New("build publication requires staged outputs, runtime inputs, retained selection and invocation identities, and an absolute record destination")
	}
	if len(files.Executions) != 0 || len(files.Derived) != 0 || len(files.Qualifications) != 0 {
		return errors.New("build publication requires fresh inputs without prior derived, render or qualification evidence")
	}
	return nil
}

func (session *SelectionSession) publishDerivedOutput(ctx context.Context, repository *remote.Repository, client *http.Client, output *derivedPublication) (string, error) {
	if err := pushDerivedOutput(ctx, repository, output); err != nil {
		return "", err
	}
	// Read back from the registry, not the local cache. A successful upload
	// status is insufficient evidence that the signed URI serves these bytes.
	reader, err := fetchArtifact(ctx, client, &output.artifact)
	if err != nil {
		return "", err
	}
	cache, err := session.openArtifactCache()
	if err != nil {
		return "", errors.Join(err, reader.Close())
	}
	path, err := retainArtifact(ctx, cache, output.artifact.Digest, reader)
	return path, errors.Join(err, reader.Close(), cache.Close())
}

func publicationRepository(reference string, client *http.Client) (*remote.Repository, error) {
	parsed, err := registry.ParseReference(reference)
	if err != nil || parsed.Reference != "" {
		return nil, errors.New("publication requires an explicit OCI registry/repository without a tag or digest")
	}
	repository, err := remote.NewRepository(reference)
	if err != nil {
		return nil, err
	}
	repository.Client, err = registryArtifactClient(client)
	return repository, err
}

func (session *SelectionSession) prepareBuildPublication(ctx context.Context, resolved *core.ResolvedComposition, staged *StagedBuild, files *DeploymentFiles, options *BuildPublicationOptions, destination *os.Root) (*PublishedBuild, []derivedPublication, error) {
	prepared, err := session.Engine.PrepareArtifactExecutions(ctx, resolved, "build", core.DeploymentInputs{})
	if err != nil {
		return nil, nil, err
	}
	if len(prepared) == 0 || len(prepared) != len(staged.Executions) {
		return nil, nil, errors.New("build evidence must cover every selected build operation exactly once")
	}
	encoded, err := json.Marshal(files)
	if err != nil {
		return nil, nil, err
	}
	result := &PublishedBuild{SelectionIdentity: resolved.Identity()}
	if err = json.Unmarshal(encoded, &result.Inputs); err != nil {
		return nil, nil, err
	}
	var outputs []derivedPublication
	var directories []string
	seen := make(map[[2]string]bool)
	for _, execution := range staged.Executions {
		key := [2]string{execution.Target, execution.Service}
		index := slices.IndexFunc(prepared, func(p *core.PreparedArtifactExecution) bool {
			r := p.Request()
			return r.Target == key[0] && r.Service == key[1]
		})
		if index < 0 || seen[key] {
			return nil, nil, errors.New("unselected or duplicate build execution")
		}
		seen[key] = true
		directory, err := isolatedExecutionDirectory(execution.Directory, directories)
		if err != nil {
			return nil, nil, err
		}
		directories = append(directories, directory)
		var receipt basev0.ArtifactExecutionReceipt
		if err = protojson.Unmarshal(execution.Receipt, &receipt); err != nil {
			return nil, nil, err
		}
		if _, err = core.VerifyArtifactExecutionDirectory(ctx, prepared[index], &receipt, directory); err != nil {
			return nil, nil, err
		}
		for _, output := range receipt.Outputs {
			publication, signed, err := session.prepareDerivedOutput(ctx, resolved, &execution, output, receipt.Identity, options, destination, len(outputs))
			if err != nil {
				return nil, nil, err
			}
			outputs = append(outputs, *publication)
			result.Inputs.Derived = append(result.Inputs.Derived, *signed)
			result.Inputs.Runtime = append(result.Inputs.Runtime, RuntimeFile{Target: execution.Target, Name: output.Name, Path: publication.path})
		}
	}
	return result, outputs, nil
}

func (session *SelectionSession) prepareDerivedOutput(ctx context.Context, resolved *core.ResolvedComposition, execution *ExecutionFiles, output *basev0.ArtifactExecutionOutput, identity string, options *BuildPublicationOptions, destination *os.Root, index int) (*derivedPublication, *core.SignedDerivedOutput, error) {
	record := resolved.Record()
	buildIndex := slices.IndexFunc(record.Builds, func(b core.BuildRequirement) bool {
		return b.Target == execution.Target && b.Service == execution.Service && b.ReplacesArtifact == output.Name
	})
	componentIndex := slices.IndexFunc(record.Components, func(c core.ResolvedComponent) bool { return c.Target == execution.Target })
	if buildIndex < 0 || componentIndex < 0 {
		return nil, nil, errors.New("build output does not identify an exact selected runtime replacement")
	}
	owner := record.Components[componentIndex].Selected.ID
	signer := options.Signers[owner]
	public := session.Engine.Trust.BuildSigners[owner][signer.Signer]
	if len(signer.Key) != ed25519.PrivateKeySize || len(public) != ed25519.PublicKeySize || !bytes.Equal(signer.Key[ed25519.SeedSize:], public) {
		return nil, nil, fmt.Errorf("%s: derived output needs its package-scoped authorized build signer", owner)
	}
	source, err := os.OpenRoot(execution.Directory)
	if err != nil {
		return nil, nil, err
	}
	file, openErr := openInputFile(ctx, source, output.Path)
	closeErr := source.Close()
	if err = errors.Join(openErr, closeErr); err != nil {
		if file != nil {
			err = errors.Join(err, file.Close())
		}
		return nil, nil, err
	}
	name := fmt.Sprintf("%04d", index)
	if err = copyApprovedInput(ctx, file, destination, name); err != nil {
		return nil, nil, err
	}
	path := filepath.Join(destination.Name(), name)
	if !artifactMatches(ctx, path, output.Digest) {
		return nil, nil, core.ErrDigestMismatch
	}
	artifact := core.ReleaseArtifact{Name: output.Name, Purpose: core.ArtifactRuntime, URI: "oci://" + options.Repository + "@" + output.Digest, Digest: output.Digest, MediaType: output.MediaType}
	statement, err := json.Marshal(core.DerivedOutput{Schema: "codefly/derived-output/v1", SelectionIdentity: resolved.Identity(), Target: execution.Target,
		Artifact: output.Name, SourceIdentity: record.Builds[buildIndex].SourceIdentity, ExecutionIdentity: identity, Digest: output.Digest, URI: artifact.URI, Signer: signer.Signer})
	if err != nil {
		return nil, nil, err
	}
	return &derivedPublication{artifact: artifact, path: path, directory: destination, name: name}, &core.SignedDerivedOutput{Statement: statement, Signature: ed25519.Sign(signer.Key, statement)}, nil
}

func (session *SelectionSession) checkPublicationInputs(ctx context.Context, snapshot *selectionSnapshot, resolved *core.ResolvedComposition, files *DeploymentFiles) error {
	inputs, closeFiles, err := openDeploymentInputs(ctx, files)
	if err != nil {
		return err
	}
	defer closeFiles()
	if _, err = session.Engine.CheckDeploymentInputs(ctx, resolved, inputs); err != nil {
		return err
	}
	return errors.Join(ctx.Err(), session.unchanged(ctx, snapshot), resolved.CheckLocalInputs())
}

func pushDerivedOutput(ctx context.Context, repository *remote.Repository, output *derivedPublication) error {
	file, err := openInputFile(ctx, output.directory, output.name)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		return errors.Join(err, file.Close())
	}
	descriptor := ocispec.Descriptor{MediaType: output.artifact.MediaType, Digest: digest.Digest(output.artifact.Digest), Size: info.Size()}
	err = repository.Push(ctx, descriptor, &approvedInputReader{ctx: ctx, reader: file})
	if err != nil {
		err = errors.Join(ctx.Err(), errors.New("derived artifact upload failed; verify registry authorization and representation"))
	}
	return errors.Join(err, file.Close())
}
