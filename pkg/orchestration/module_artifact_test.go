package orchestration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/codefly-dev/cli/pkg/composition"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// artifactWorkspace is a one-module composition selected by an explicit
// committed path: a developer checkout, which legitimately has no
// materialization receipt, so acquisition succeeds and its provenance is local
// development evidence rather than release authenticity.
func artifactWorkspace(t *testing.T) *resources.Workspace {
	t.Helper()
	return writeTempWorkspace(t, map[string]string{
		"workspace.codefly.yaml": `name: artifact-export
layout: modules
modules:
    - name: wiki
      path: modules/wiki
`,
		"modules/wiki/module.codefly.yaml":                   excludedRootExportModule,
		"modules/wiki/services/backend/service.codefly.yaml": excludedRootExportService,
	})
}

func artifactReference(module, path string) *basev0.ConfigurationModuleArtifactReference {
	return &basev0.ConfigurationModuleArtifactReference{Module: module, Path: path}
}

// TestTheResolverAcquiresTheSelectedModulesExactBytes is the delivery truth:
// whitespace and newlines included, byte for byte, with no reserialization.
func TestTheResolverAcquiresTheSelectedModulesExactBytes(t *testing.T) {
	ctx := context.Background()
	workspace := artifactWorkspace(t)
	body := "# Install\n\n  indented\ttab\n\nlast line without newline"
	require.NoError(t, os.MkdirAll(filepath.Join(workspace.Dir(), "modules/wiki/docs"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(workspace.Dir(), "modules/wiki/docs/install.md"), []byte(body), 0o600))

	content, err := ModuleArtifactResolverFor(workspace)(ctx, artifactReference("wiki", "docs/install.md"))
	require.NoError(t, err)
	require.Equal(t, body, string(content))
}

// TestTheResolverRefusesWhatItCannotConfineOrBound covers the acquisition
// obligations core hands to the CLI. Every one of these is a real file or a
// real path, not a stub: a string prefix check followed by an unrestricted
// open passes several of them.
func TestTheResolverRefusesWhatItCannotConfineOrBound(t *testing.T) {
	ctx := context.Background()
	workspace := artifactWorkspace(t)
	module := filepath.Join(workspace.Dir(), "modules/wiki")
	require.NoError(t, os.MkdirAll(filepath.Join(module, "docs"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(module, "docs/install.md"), []byte("ok\n"), 0o600))

	// A secret parked beside the composition, which a traversal or a symlink
	// out of the module would otherwise deliver as public configuration.
	require.NoError(t, os.WriteFile(filepath.Join(workspace.Dir(), "outside.txt"), []byte("not-public\n"), 0o600))
	require.NoError(t, os.Symlink(filepath.Join(workspace.Dir(), "outside.txt"), filepath.Join(module, "docs/escape.md")))
	require.NoError(t, os.Symlink("../../../outside.txt", filepath.Join(module, "docs/relative-escape.md")))
	require.NoError(t, syscall.Mkfifo(filepath.Join(module, "docs/pipe"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(module, "docs/oversize.md"),
		[]byte(strings.Repeat("x", resources.FileCarrierThreshold+1)), 0o600))

	resolve := ModuleArtifactResolverFor(workspace)
	for _, probe := range []struct {
		name string
		ref  *basev0.ConfigurationModuleArtifactReference
	}{
		{"an absolute path", artifactReference("wiki", filepath.Join(workspace.Dir(), "outside.txt"))},
		{"a traversing path", artifactReference("wiki", "../../outside.txt")},
		{"an absolute symlink out of the module", artifactReference("wiki", "docs/escape.md")},
		{"a relative symlink out of the module", artifactReference("wiki", "docs/relative-escape.md")},
		{"a FIFO, which would block the read", artifactReference("wiki", "docs/pipe")},
		{"a directory", artifactReference("wiki", "docs")},
		{"a missing file", artifactReference("wiki", "docs/absent.md")},
		{"an empty path", artifactReference("wiki", "")},
		{"a module the composition does not name", artifactReference("ghost", "docs/install.md")},
	} {
		t.Run(probe.name, func(t *testing.T) {
			content, err := resolve(ctx, probe.ref)
			require.Error(t, err)
			require.Empty(t, content)
			require.NotContains(t, err.Error(), "not-public", "the refusal never carries the content it refused")
		})
	}

	// One byte over core's inclusive bound is acquired, then refused by core's
	// own byte admission — the CLI reads the extra byte precisely so the
	// refusal can be made rather than silently truncating.
	oversize, err := resolve(ctx, artifactReference("wiki", "docs/oversize.md"))
	require.NoError(t, err)
	require.Len(t, oversize, resources.FileCarrierThreshold+1)
	require.Error(t, resources.ValidateModuleArtifactBytes(string(oversize)))

	// And the inclusive threshold itself is admitted.
	require.NoError(t, os.WriteFile(filepath.Join(module, "docs/exact.md"),
		[]byte(strings.Repeat("x", resources.FileCarrierThreshold)), 0o600))
	exact, err := resolve(ctx, artifactReference("wiki", "docs/exact.md"))
	require.NoError(t, err)
	require.NoError(t, resources.ValidateModuleArtifactBytes(string(exact)))
}

// TestTheResolverRefusesAnUnmaterializedModule is the receipt half seen from
// the consuming side: a composed module this machine has not materialized is
// refused rather than fetched behind the read, and no literal stands in.
func TestTheResolverRefusesAnUnmaterializedModule(t *testing.T) {
	ctx := context.Background()
	workspace := writeTempWorkspace(t, map[string]string{
		"workspace.codefly.yaml": `name: artifact-pinned
layout: modules
modules:
    - name: wiki
      source: github.com/codefly-dev/absent
      version: 0.0.1
`,
	})
	content, err := ModuleArtifactResolverFor(workspace)(ctx, artifactReference("wiki", "docs/install.md"))
	require.Error(t, err)
	require.Empty(t, content)
}

// TestTheDeliveryBaselinePreservesEveryOffer is what makes core's collision
// check possible: the offer precedence would discard is exactly the one that
// must still be visible, so nothing is merged, deduplicated or keyed first.
func TestTheDeliveryBaselinePreservesEveryOffer(t *testing.T) {
	value := func(key, text string) *basev0.ConfigurationValue {
		return &basev0.ConfigurationValue{Key: key, Value: text}
	}
	own := &basev0.Configuration{Origin: "wiki/backend", Infos: []*basev0.ConfigurationInformation{{
		Name: "docs", ConfigurationValues: []*basev0.ConfigurationValue{value("README", "a")},
	}}}
	first := &basev0.Configuration{Origin: resources.ConfigurationWorkspace, Infos: []*basev0.ConfigurationInformation{{
		Name: "docs", ConfigurationValues: []*basev0.ConfigurationValue{value("README", "b")},
	}}}
	// The same pointer offered twice, and a duplicate coordinate: both survive.
	baseline := deliveryBaseline(own, []*basev0.Configuration{first, first}, []*basev0.Configuration{first})
	require.Len(t, baseline, 4)
	require.Same(t, own, baseline[0])
	for _, conf := range baseline[1:] {
		require.Same(t, first, conf)
	}
	require.Empty(t, deliveryBaseline(nil, nil, nil))
}

// TestTheRetainedDeliveryIsDetached locks what Start compares against. The
// retained payload must not move when the slices it was gathered from are
// mutated by a later load, and the generated artifact evidence must travel
// with each value — a copy that dropped it would protect nothing.
func TestTheRetainedDeliveryIsDetached(t *testing.T) {
	body := "# Install\n"
	digest := sha256.Sum256([]byte(body))
	evidence := &basev0.ResolvedConfigurationModuleArtifact{
		Reference: artifactReference("wiki", "docs/install.md"),
		ArtifactIdentity: &basev0.SourceIdentity{
			Algorithm: basev0.SourceIdentityAlgorithm_SOURCE_IDENTITY_ALGORITHM_SHA256,
			Digest:    hex.EncodeToString(digest[:]),
			SizeBytes: int64(len(body)),
		},
	}
	source := []*basev0.Configuration{nil, {Origin: resources.ConfigurationWorkspace, Infos: []*basev0.ConfigurationInformation{{
		Name: "docs", ConfigurationValues: []*basev0.ConfigurationValue{
			{Key: "README", Value: body, ResolvedModuleArtifact: evidence},
		},
	}}}}

	detached := detachConfigurations(source)
	require.Len(t, detached, 1, "a nil offer carries nothing to retain")
	retained := detached[0].Infos[0].ConfigurationValues[0]
	require.Equal(t, body, retained.Value)
	require.NotNil(t, retained.ResolvedModuleArtifact, "the evidence travels with the copy")
	require.NotSame(t, evidence, retained.ResolvedModuleArtifact, "and it is a clone, not the same message")
	require.NoError(t, resources.ValidateModuleArtifactValue(retained))

	source[1].Infos[0].ConfigurationValues[0].Value = "rewritten"
	require.Equal(t, body, detached[0].Infos[0].ConfigurationValues[0].Value,
		"the retained delivery is what the service received, whatever happens to the slices it came from")
}

// TestTheSelectionIsComparedWholeNotByPath is the case a path-only recheck
// cannot see.
//
// A non-managed selection — an explicit overlay path, carrying no receipt —
// resolves through the overlay snapshot the workspace was LOADED with, so it
// answers the same directory on every ask even after the overlay on disk has
// been re-pointed. Comparing directories across the read therefore passes
// while the selection has actually moved. The descriptor carries the disk-read
// directive beside the resolved directory, so the move is visible.
func TestTheSelectionIsComparedWholeNotByPath(t *testing.T) {
	ctx := context.Background()
	workspace := writeTempWorkspace(t, map[string]string{
		"workspace.codefly.yaml": `name: artifact-reselect
layout: modules
modules:
    - name: wiki
      path: modules/wiki
`,
		"modules/wiki/module.codefly.yaml":                   excludedRootExportModule,
		"modules/wiki/services/backend/service.codefly.yaml": excludedRootExportService,
		"modules/wiki/docs/install.md":                       "A\n",
		"codefly.local.yaml":                                 "resolve:\n    wiki:\n        path: modules/wiki\n",
	})

	first, err := composition.SelectModule(ctx, workspace, "wiki")
	require.NoError(t, err)
	require.False(t, first.Verified, "an explicit overlay path is local development evidence")
	require.Equal(t, "modules/wiki", first.DirectivePath)

	// Re-point the module on disk, to a directory that resolves the same way
	// through the loaded snapshot.
	require.NoError(t, os.MkdirAll(filepath.Join(workspace.Dir(), "elsewhere/wiki/docs"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(workspace.Dir(), "codefly.local.yaml"),
		[]byte("resolve:\n    wiki:\n        path: elsewhere/wiki\n"), 0o600))

	second, err := composition.SelectModule(ctx, workspace, "wiki")
	require.NoError(t, err)
	require.NotEqual(t, *first, *second, "the selection moved, and the descriptor says so")
	require.Equal(t, "elsewhere/wiki", second.DirectivePath, "which the disk-read directive is what carries")
	require.Equal(t, first.Dir, second.Dir,
		"while the RESOLVED directory is unchanged — which is exactly why a path-only recheck passed")
}
