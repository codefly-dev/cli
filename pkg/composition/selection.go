package composition

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"time"

	"github.com/codefly-dev/cli/pkg/gh"
	"github.com/codefly-dev/cli/pkg/internal/selectionguard"
	core "github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	"github.com/gofrs/flock"
	"gopkg.in/yaml.v3"
)

const (
	SelectionFile      = selectionguard.SelectionFile
	LocalSelectionFile = ".codefly/composition-local.json"
)

// SelectionInputs is transport for Core's inputs, not a second resolution model.
// Local checkouts are deliberately kept in a separate, machine-local file.
type SelectionInputs struct {
	Root         core.ReleaseSelection  `json:"root"`
	SourceBuilds []string               `json:"sourceBuilds,omitempty"`
	Artifacts    []core.ArtifactRequest `json:"artifacts,omitempty"`
}

type SelectionInspection struct {
	Identity   string                             `json:"identity"`
	Resolution core.ResolutionRecord              `json:"resolution"`
	Checkouts  map[string]LocalCheckoutInspection `json:"checkouts,omitempty"`
}

type LocalCheckoutInspection struct {
	Description *CheckoutDescription `json:"description,omitempty"`
	Dirty       *bool                `json:"dirty"`
}

type SelectionSession struct {
	Root                  string
	Engine                *core.Engine
	ConfigurationIdentity string
	trustPath             string
	trustDocument         []byte
}

// NewSelectionSession never materializes a module archive or a source checkout.
func NewSelectionSession(workspace, product, configurationIdentity string) (*SelectionSession, error) {
	trustPath, err := filepath.Abs(filepath.Join(workspace, resources.WorkspaceConfigurationName))
	if err != nil {
		return nil, err
	}
	before, err := os.ReadFile(trustPath)
	if err != nil {
		return nil, err
	}
	trust, _, err := LoadModuleTrust(workspace)
	if err != nil {
		return nil, err
	}
	if trust == nil {
		return nil, errors.New("composition selection requires package-scoped module-trust")
	}
	after, err := os.ReadFile(trustPath)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(before, after) {
		return nil, errors.New("workspace trust changed while opening selection session")
	}
	client, err := gh.NewClient()
	if err != nil {
		return nil, err
	}
	packages := make(map[string]core.GitHubPackage)
	for id, repository := range trust.Repositories {
		owner, name, splitErr := splitGitHubRepository(repository)
		if splitErr != nil {
			return nil, splitErr
		}
		packages[id] = core.GitHubPackage{Owner: owner, RepositoryName: name, RepositoryURL: repository,
			ArtifactAsset: "module.tar", MetadataAsset: core.PackageManifestFileName,
			ProvenanceAsset: "provenance.json", SignatureAsset: "provenance.sig"}
	}
	root, err := filepath.Abs(product)
	if err != nil {
		return nil, err
	}
	return &SelectionSession{Root: root, ConfigurationIdentity: configurationIdentity,
		trustPath: trustPath, trustDocument: after,
		Engine: core.NewEngine(root, core.NewGitHubResolver(client, nil, packages), *trust)}, nil
}

type selectionSnapshot struct {
	descriptor *core.Descriptor
	inputs     SelectionInputs
	local      map[string]string
	files      map[string][]byte
}

func (session *SelectionSession) locked(ctx context.Context, run func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !filepath.IsAbs(session.Root) || session.Engine == nil {
		return errors.New("absolute product root and Core engine are required")
	}
	lock := flock.New(filepath.Join(session.Root, ".codefly-composition.lock"))
	defer func() { _ = lock.Close() }()
	locked, err := lock.TryLockContext(ctx, 50*time.Millisecond)
	if err != nil {
		return err
	}
	if !locked {
		return ErrLockTimeout
	}
	defer func() { _ = lock.Unlock() }()
	if err := ctx.Err(); err != nil {
		return err
	}
	return run()
}

func decodeSelectionJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("unexpected trailing JSON")
	}
	return nil
}

func (session *SelectionSession) snapshot(initial bool) (*selectionSnapshot, error) {
	snapshot := &selectionSnapshot{local: map[string]string{}, files: map[string][]byte{}}
	for _, name := range []string{core.DescriptorFileName, SelectionFile, LocalSelectionFile} {
		data, err := os.ReadFile(filepath.Join(session.Root, name))
		optional := name == LocalSelectionFile || (initial && name == SelectionFile)
		if err != nil && (!os.IsNotExist(err) || !optional) {
			return nil, err
		}
		snapshot.files[name] = data
	}
	decoder := yaml.NewDecoder(bytes.NewReader(snapshot.files[core.DescriptorFileName]))
	decoder.KnownFields(true)
	if err := decoder.Decode(&snapshot.descriptor); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("unexpected trailing descriptor document")
	}
	if err := snapshot.descriptor.Validate(); err != nil {
		return nil, err
	}
	if data := snapshot.files[SelectionFile]; data != nil {
		if initial {
			return nil, errors.New("release selection already exists; initialization cannot overwrite it")
		}
		if err := decodeSelectionJSON(data, &snapshot.inputs); err != nil {
			return nil, err
		}
	}
	if data := snapshot.files[LocalSelectionFile]; data != nil {
		if err := decodeSelectionJSON(data, &snapshot.local); err != nil {
			return nil, err
		}
		if snapshot.local == nil {
			return nil, errors.New("local selections must be an object, not null")
		}
	}
	return snapshot, nil
}

func (session *SelectionSession) options(snapshot *selectionSnapshot) core.ResolutionOptions {
	return core.ResolutionOptions{ProductRoot: session.Root, ConfigurationIdentity: session.ConfigurationIdentity,
		LocalCheckouts: snapshot.local, SourceBuilds: snapshot.inputs.SourceBuilds, Artifacts: snapshot.inputs.Artifacts}
}

func (session *SelectionSession) resolve(ctx context.Context, snapshot *selectionSnapshot) (*core.ResolvedComposition, error) {
	return session.Engine.ResolveComposition(ctx, snapshot.descriptor, snapshot.inputs.Root, session.options(snapshot))
}

func (session *SelectionSession) unchanged(snapshot *selectionSnapshot) error {
	if session.trustPath != "" {
		current, err := os.ReadFile(session.trustPath)
		if err != nil {
			return err
		}
		if !bytes.Equal(current, session.trustDocument) {
			return errors.New("workspace trust changed; reopen the selection session")
		}
	}
	for name, before := range snapshot.files {
		after, err := os.ReadFile(filepath.Join(session.Root, name))
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if (before == nil) != (after == nil) || !bytes.Equal(before, after) {
			return fmt.Errorf("%s changed during resolution; no selection was written", name)
		}
	}
	return nil
}

func inspectSelection(ctx context.Context, resolved *core.ResolvedComposition, local map[string]string) SelectionInspection {
	inspection := SelectionInspection{Identity: resolved.Identity(), Resolution: resolved.Record(), Checkouts: map[string]LocalCheckoutInspection{}}
	for target, path := range local {
		// Missing Git metadata is not a clean checkout. Core's content digest is
		// authoritative even for a directory without Git metadata.
		description, _ := DescribeCheckout(ctx, path)
		checkout := LocalCheckoutInspection{Description: description}
		// Disable repository-configured fsmonitor execution and index writes.
		command := exec.CommandContext(ctx, "git", "-C", path, "-c", "core.fsmonitor=false", "-c", "core.untrackedCache=false", "status", "--porcelain=v1", "-z", "--untracked-files=all")
		command.Env = append(command.Environ(), "GIT_OPTIONAL_LOCKS=0")
		if output, err := command.Output(); err == nil {
			dirty := len(output) != 0
			checkout.Dirty = &dirty
		}
		inspection.Checkouts[target] = checkout
	}
	return inspection
}

func (session *SelectionSession) Initialize(ctx context.Context, inputs *SelectionInputs) (SelectionInspection, error) {
	var result SelectionInspection
	if inputs == nil {
		return result, errors.New("release selection inputs are required")
	}
	err := session.locked(ctx, func() error {
		snapshot, err := session.snapshot(true)
		if err != nil {
			return err
		}
		snapshot.inputs = *inputs
		if validationErr := session.validateReleases(ctx, snapshot); validationErr != nil {
			return validationErr
		}
		resolved, err := session.resolve(ctx, snapshot)
		if err != nil {
			return err
		}
		if err := session.unchanged(snapshot); err != nil {
			return err
		}
		if err := session.writeJSON(ctx, SelectionFile, inputs); err != nil {
			return err
		}
		result = inspectSelection(ctx, resolved, snapshot.local)
		return nil
	})
	return result, err
}

// InitializeVersion obtains the immutable digest from release provenance. The
// candidate is not trusted or persisted until Initialize authenticates it.
func (session *SelectionSession) InitializeVersion(ctx context.Context, version string) (SelectionInspection, error) {
	descriptor, err := core.LoadDescriptor(session.Root)
	if err != nil {
		return SelectionInspection{}, err
	}
	release, err := session.candidateRelease(ctx, descriptor.Base.ID, version)
	if err != nil {
		return SelectionInspection{}, err
	}
	return session.Initialize(ctx, &SelectionInputs{Root: release})
}

func (session *SelectionSession) candidateRelease(ctx context.Context, id, version string) (core.ReleaseSelection, error) {
	resolver, ok := session.Engine.Resolver.(core.MetadataResolver)
	if !ok {
		return core.ReleaseSelection{}, errors.New("resolver cannot acquire signed metadata")
	}
	metadata, err := resolver.ResolveMetadata(ctx, core.ResolveRequest{Package: id, Version: version})
	if err != nil {
		return core.ReleaseSelection{}, err
	}
	if metadata == nil {
		return core.ReleaseSelection{}, errors.New("release metadata is missing")
	}
	provenance, err := core.ParseProvenance(metadata.Provenance)
	if err != nil {
		return core.ReleaseSelection{}, err
	}
	return core.ReleaseSelection{ID: id, Version: version, Digest: provenance.ArtifactDigest}, nil
}

func (session *SelectionSession) SelectVersion(ctx context.Context, target, version, rationale string) (SelectionInspection, error) {
	var replacement core.Replacement
	var expected string
	err := session.withResolved(ctx, func(_ *selectionSnapshot, resolved *core.ResolvedComposition) error {
		components := resolved.Record().Components
		for i := range components {
			component := &components[i]
			if component.Target != target {
				continue
			}
			release, err := session.candidateRelease(ctx, component.Selected.ID, version)
			if err != nil {
				return err
			}
			replacement = core.Replacement{Target: target, Release: release, Rationale: rationale, Requirements: component.AdditionalRequirements}
			expected = resolved.Identity()
			return nil
		}
		return fmt.Errorf("%s: replacement target does not participate in the composition", target)
	})
	if err != nil {
		return SelectionInspection{}, err
	}
	return session.selectExpected(ctx, &replacement, expected)
}

func (session *SelectionSession) Inspect(ctx context.Context) (SelectionInspection, error) {
	var result SelectionInspection
	err := session.withResolved(ctx, func(snapshot *selectionSnapshot, resolved *core.ResolvedComposition) error {
		result = inspectSelection(ctx, resolved, snapshot.local)
		return nil
	})
	return result, err
}

func (session *SelectionSession) validateReleases(ctx context.Context, snapshot *selectionSnapshot) error {
	options := session.options(snapshot)
	options.LocalCheckouts = nil
	_, err := session.Engine.ResolveComposition(ctx, snapshot.descriptor, snapshot.inputs.Root, options)
	return err
}

func (session *SelectionSession) withResolved(ctx context.Context, run func(*selectionSnapshot, *core.ResolvedComposition) error) error {
	return session.locked(ctx, func() error {
		snapshot, err := session.snapshot(false)
		if err != nil {
			return err
		}
		resolved, err := session.resolve(ctx, snapshot)
		if err != nil {
			return err
		}
		if err := session.unchanged(snapshot); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return run(snapshot, resolved)
	})
}

func (session *SelectionSession) Select(ctx context.Context, replacement *core.Replacement) (SelectionInspection, error) {
	return session.selectExpected(ctx, replacement, "")
}

func (session *SelectionSession) selectExpected(ctx context.Context, replacement *core.Replacement, expected string) (SelectionInspection, error) {
	var result SelectionInspection
	if replacement == nil {
		return result, errors.New("replacement is required")
	}
	err := session.locked(ctx, func() error {
		snapshot, err := session.snapshot(false)
		if err != nil {
			return err
		}
		if expected != "" {
			current, resolveErr := session.resolve(ctx, snapshot)
			if resolveErr != nil {
				return resolveErr
			}
			if current.Identity() != expected {
				return errors.New("selection changed while acquiring candidate metadata; retry against current inputs")
			}
		}
		index := slices.IndexFunc(snapshot.descriptor.Replacements, func(value core.Replacement) bool { return value.Target == replacement.Target })
		if index < 0 {
			snapshot.descriptor.Replacements = append(snapshot.descriptor.Replacements, *replacement)
		} else {
			snapshot.descriptor.Replacements[index] = *replacement
		}
		if validationErr := session.validateReleases(ctx, snapshot); validationErr != nil {
			return validationErr
		}
		resolved, err := session.resolve(ctx, snapshot)
		if err != nil {
			return err
		}
		if changedErr := session.unchanged(snapshot); changedErr != nil {
			return changedErr
		}
		// Replace only the selections node, preserving comments and unrelated
		// descriptor fields instead of serializing a second product model.
		var document, replacements yaml.Node
		if decodeErr := yaml.Unmarshal(snapshot.files[core.DescriptorFileName], &document); decodeErr != nil {
			return decodeErr
		}
		if encodeErr := replacements.Encode(snapshot.descriptor.Replacements); encodeErr != nil {
			return encodeErr
		}
		mapping := document.Content[0]
		found := false
		for i := 0; i < len(mapping.Content); i += 2 {
			if mapping.Content[i].Value == "replacements" {
				mapping.Content[i+1] = &replacements
				found = true
				break
			}
		}
		if !found {
			mapping.Content = append(mapping.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "replacements"}, &replacements)
		}
		data, err := yaml.Marshal(&document)
		if err != nil {
			return err
		}
		if err := resolved.CheckLocalInputs(); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := shared.WriteFileAtomic(ctx, filepath.Join(session.Root, core.DescriptorFileName), data, 0o644); err != nil {
			return err
		}
		result = inspectSelection(ctx, resolved, snapshot.local)
		return nil
	})
	return result, err
}

// Develop adds independent checkouts. An empty path removes only that local
// substitution; the committed release replacement and checkout are untouched.
func (session *SelectionSession) Develop(ctx context.Context, changes map[string]string) (SelectionInspection, error) {
	var result SelectionInspection
	err := session.locked(ctx, func() error {
		snapshot, err := session.snapshot(false)
		if err != nil {
			return err
		}
		for target, path := range changes {
			if path == "" {
				if _, exists := snapshot.local[target]; !exists {
					return fmt.Errorf("%s: no local substitution to restore", target)
				}
				delete(snapshot.local, target)
			} else {
				if !filepath.IsAbs(path) {
					return fmt.Errorf("%s: checkout path must be absolute", target)
				}
				snapshot.local[target] = path
			}
		}
		resolved, err := session.resolve(ctx, snapshot)
		if err != nil {
			return err
		}
		if err := session.unchanged(snapshot); err != nil {
			return err
		}
		if err := session.writeJSON(ctx, LocalSelectionFile, snapshot.local); err != nil {
			return err
		}
		result = inspectSelection(ctx, resolved, snapshot.local)
		return nil
	})
	return result, err
}

func (session *SelectionSession) Upstream(ctx context.Context) ([]core.UpstreamAdoption, error) {
	var result []core.UpstreamAdoption
	err := session.withResolved(ctx, func(_ *selectionSnapshot, resolved *core.ResolvedComposition) error {
		var err error
		result, err = resolved.UpstreamAdoptions(nil)
		return err
	})
	return result, err
}

func (session *SelectionSession) ProposeRemoval(ctx context.Context, candidate core.ReleaseSelection, target string) (*core.OverrideRemoval, error) {
	var result *core.OverrideRemoval
	err := session.withResolved(ctx, func(snapshot *selectionSnapshot, resolved *core.ResolvedComposition) error {
		var err error
		result, err = session.Engine.ProposeOverrideRemoval(ctx, resolved, snapshot.descriptor, candidate, session.options(snapshot), target)
		return err
	})
	return result, err
}

func (session *SelectionSession) writeJSON(ctx context.Context, name string, value any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(session.Root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return shared.WriteFileAtomic(ctx, path, append(data, '\n'), 0o600)
}
