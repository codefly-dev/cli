package runnable

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func digestOfBytes(content []byte) string {
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func writeFile(t *testing.T, path string, content []byte) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	require.NoError(t, os.WriteFile(path, content, 0600))
	return digestOfBytes(content)
}

// An agent that packaged a stale snapshot reports evidence that is perfectly
// self-consistent: the handler digest it claims matches the archive it built.
// Only the author's source on disk disproves it, which is why the CLI hashes
// it instead of trusting the process it exists to verify.
func TestVerifyBuildInputsRejectsEvidenceThatDoesNotMatchTheSource(t *testing.T) {
	dir := t.TempDir()
	handlerDigest := writeFile(t, filepath.Join(dir, "handler.py"), []byte("def handle(context, input):\n    return {}\n"))
	inputDigest := writeFile(t, filepath.Join(dir, "lib", "helper.py"), []byte("VALUE = 1\n"))
	inputs := []string{"lib/helper.py"}

	build := func(handler string, input string) *basev0.RunnableBuild {
		return &basev0.RunnableBuild{
			Handler: &basev0.RunnableInputDigest{Path: "handler.py", Digest: handler},
			Inputs:  []*basev0.RunnableInputDigest{{Path: "lib/helper.py", Digest: input}},
		}
	}

	require.NoError(t, verifyBuildInputs(dir, build(handlerDigest, inputDigest), "handler.py", inputs))

	stale := digestOfBytes([]byte("def handle(context, input):\n    return {\"old\": True}\n"))
	require.ErrorContains(t, verifyBuildInputs(dir, build(stale, inputDigest), "handler.py", inputs),
		"handler handler.py digest")
	require.ErrorContains(t, verifyBuildInputs(dir, build(handlerDigest, digestOfBytes([]byte("VALUE = 2\n"))), "handler.py", inputs),
		"build input lib/helper.py digest")

	// The author edits the handler; a cached agent keeps reporting the old
	// evidence. Path checks alone pass, digests catch it.
	writeFile(t, filepath.Join(dir, "handler.py"), []byte("def handle(context, input):\n    return {\"new\": True}\n"))
	require.ErrorContains(t, verifyBuildInputs(dir, build(handlerDigest, inputDigest), "handler.py", inputs),
		"packaged different bytes than the declaration references")
}

func TestVerifyBuildInputsRejectsPathAndShapeMismatches(t *testing.T) {
	dir := t.TempDir()
	handlerDigest := writeFile(t, filepath.Join(dir, "handler.py"), []byte("x\n"))
	handler := &basev0.RunnableInputDigest{Path: "handler.py", Digest: handlerDigest}

	require.ErrorContains(t, verifyBuildInputs(dir, &basev0.RunnableBuild{Handler: handler}, "other.py", nil),
		"handler differs from the declaration")
	require.ErrorContains(t, verifyBuildInputs(dir, &basev0.RunnableBuild{
		Handler: handler,
		Inputs:  []*basev0.RunnableInputDigest{{Path: "extra.py", Digest: handlerDigest}},
	}, "handler.py", nil), "build input paths differ from the declaration")
	require.ErrorContains(t, verifyBuildInputs(dir, &basev0.RunnableBuild{Handler: handler}, "handler.py", []string{"missing.py"}),
		"build input paths differ from the declaration")
	require.ErrorContains(t, verifyBuildInputs("", &basev0.RunnableBuild{Handler: handler}, "handler.py", nil),
		"not loaded from a directory")

	// A declared input that vanished from the source must fail loudly rather
	// than be silently treated as unchanged.
	missingDigest := writeFile(t, filepath.Join(dir, "gone.py"), []byte("y\n"))
	require.NoError(t, os.Remove(filepath.Join(dir, "gone.py")))
	require.ErrorContains(t, verifyBuildInputs(dir, &basev0.RunnableBuild{
		Handler: handler,
		Inputs:  []*basev0.RunnableInputDigest{{Path: "gone.py", Digest: missingDigest}},
	}, "handler.py", []string{"gone.py"}), "verify build input gone.py against the source")
}

// artifactFixture writes one archive and returns the matching descriptor and
// emitted metadata, so each test below perturbs exactly one thing.
func artifactFixture(t *testing.T, dir string, name string, content []byte) (*basev0.RunnableArtifact, *builderv0.PackageArtifact) {
	t.Helper()
	digest := writeFile(t, filepath.Join(dir, name), content)
	platform := runtime.GOOS + "/" + runtime.GOARCH
	command := []string{"bin/python", "-m", "codefly_runnable"}
	return &basev0.RunnableArtifact{
		Kind: basev0.RunnableArtifact_NATIVE, Platform: platform,
		Reference: name, Digest: digest, Command: command,
	}, &builderv0.PackageArtifact{
		Kind: builderv0.PackageArtifact_ARCHIVE, Path: filepath.Join(dir, name),
		Target: &builderv0.PackageTarget{Os: runtime.GOOS, Architecture: runtime.GOARCH},
		Sha256: digest[len("sha256:"):], Command: command,
	}
}

// The artifact root is symlink-resolved, so an agent-reported path must be put
// in the same form before comparison. Without that, any build output reached
// through a symlink (macOS /tmp -> /private/tmp) fails with an error that
// blames the agent for a CLI path-normalization bug.
func TestVerifyArtifactsAcceptsASymlinkedOutputDirectory(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real", "artifacts")
	require.NoError(t, os.MkdirAll(real, 0700))
	link := filepath.Join(root, "link")
	require.NoError(t, os.Symlink(filepath.Join(root, "real"), link))

	// The agent is handed the symlinked path and echoes back a path under it.
	artifact, emitted := artifactFixture(t, real, "word-count-0.0.1.tar.gz", []byte("archive bytes"))
	emitted.Path = filepath.Join(link, "artifacts", "word-count-0.0.1.tar.gz")

	require.NoError(t, verifyArtifacts([]*basev0.RunnableArtifact{artifact}, []*builderv0.PackageArtifact{emitted}, filepath.Join(link, "artifacts")))
}

func TestVerifyArtifactsRejectsTamperedAndMismatchedArtifacts(t *testing.T) {
	for _, test := range []struct {
		name    string
		perturb func(t *testing.T, dir string, artifact *basev0.RunnableArtifact, emitted *builderv0.PackageArtifact)
		message string
	}{
		{"content changed after packaging", func(t *testing.T, dir string, artifact *basev0.RunnableArtifact, _ *builderv0.PackageArtifact) {
			require.NoError(t, os.WriteFile(filepath.Join(dir, artifact.GetReference()), []byte("changed"), 0600))
		}, "content digest does not match"},
		{"emitted digest disagrees", func(_ *testing.T, _ string, _ *basev0.RunnableArtifact, emitted *builderv0.PackageArtifact) {
			emitted.Sha256 = digestOfBytes([]byte("other"))[len("sha256:"):]
		}, "does not match the emitted file metadata"},
		{"emitted platform disagrees", func(_ *testing.T, _ string, _ *basev0.RunnableArtifact, emitted *builderv0.PackageArtifact) {
			emitted.Target = &builderv0.PackageTarget{Os: "plan9", Architecture: "mips"}
		}, "does not match the emitted file metadata"},
		{"emitted command disagrees", func(_ *testing.T, _ string, _ *basev0.RunnableArtifact, emitted *builderv0.PackageArtifact) {
			emitted.Command = []string{"sh", "-c", "curl evil"}
		}, "does not match the emitted file metadata"},
		{"emitted path points elsewhere", func(_ *testing.T, dir string, _ *basev0.RunnableArtifact, emitted *builderv0.PackageArtifact) {
			emitted.Path = filepath.Join(dir, "somewhere-else.tar.gz")
		}, "does not match the emitted file metadata"},
		{"artifact kind is not native", func(_ *testing.T, _ string, artifact *basev0.RunnableArtifact, _ *builderv0.PackageArtifact) {
			artifact.Kind = basev0.RunnableArtifact_IMAGE
		}, "unsupported artifact kind"},
		{"reference escapes the directory", func(_ *testing.T, _ string, artifact *basev0.RunnableArtifact, _ *builderv0.PackageArtifact) {
			artifact.Reference = "../escape.tar.gz"
		}, "invalid or duplicate native artifact filename"},
		{"reference is the descriptor", func(_ *testing.T, _ string, artifact *basev0.RunnableArtifact, _ *builderv0.PackageArtifact) {
			artifact.Reference = PackageFile
		}, "invalid or duplicate native artifact filename"},
		{"reference is a directory component", func(_ *testing.T, _ string, artifact *basev0.RunnableArtifact, _ *builderv0.PackageArtifact) {
			artifact.Reference = "nested/archive.tar.gz"
		}, "invalid or duplicate native artifact filename"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			artifact, emitted := artifactFixture(t, dir, "word-count-0.0.1.tar.gz", []byte("archive bytes"))
			test.perturb(t, dir, artifact, emitted)
			require.ErrorContains(t, verifyArtifacts([]*basev0.RunnableArtifact{artifact}, []*builderv0.PackageArtifact{emitted}, dir), test.message)
		})
	}
}

func TestVerifyArtifactsRejectsSymlinkedAndIrregularArtifacts(t *testing.T) {
	dir := t.TempDir()
	artifact, emitted := artifactFixture(t, dir, "word-count-0.0.1.tar.gz", []byte("archive bytes"))
	// Replace the regular file with a symlink to identical content: the bytes
	// would hash correctly, but a package must pin a real file.
	elsewhere := filepath.Join(t.TempDir(), "elsewhere.tar.gz")
	require.NoError(t, os.WriteFile(elsewhere, []byte("archive bytes"), 0600))
	require.NoError(t, os.Remove(filepath.Join(dir, artifact.GetReference())))
	require.NoError(t, os.Symlink(elsewhere, filepath.Join(dir, artifact.GetReference())))
	require.ErrorContains(t, verifyArtifacts([]*basev0.RunnableArtifact{artifact}, []*builderv0.PackageArtifact{emitted}, dir), "is not a regular file")
}

func TestVerifyArtifactsRejectsCountAndDuplicateMismatches(t *testing.T) {
	dir := t.TempDir()
	artifact, emitted := artifactFixture(t, dir, "word-count-0.0.1.tar.gz", []byte("archive bytes"))

	require.ErrorContains(t, verifyArtifacts([]*basev0.RunnableArtifact{artifact},
		[]*builderv0.PackageArtifact{emitted, emitted}, dir), "package descriptor and emitted artifacts differ")
	require.ErrorContains(t, verifyArtifacts([]*basev0.RunnableArtifact{artifact, artifact},
		[]*builderv0.PackageArtifact{emitted, emitted}, dir), "invalid or duplicate native artifact filename")
}

// Everything the descriptor names gets digested; a file the agent never
// reported carries no evidence at all, so the directory must not present it as
// part of a verified package.
func TestVerifyArtifactsRejectsUnreportedFiles(t *testing.T) {
	dir := t.TempDir()
	artifact, emitted := artifactFixture(t, dir, "word-count-0.0.1.tar.gz", []byte("archive bytes"))
	artifacts := []*basev0.RunnableArtifact{artifact}
	emittedFiles := []*builderv0.PackageArtifact{emitted}

	require.NoError(t, verifyArtifacts(artifacts, emittedFiles, dir))

	// The descriptor Build writes after verification stays acceptable, so
	// re-verifying an existing build directory keeps working.
	writeFile(t, filepath.Join(dir, PackageFile), []byte("{}\n"))
	require.NoError(t, verifyArtifacts(artifacts, emittedFiles, dir))

	writeFile(t, filepath.Join(dir, "stowaway.sh"), []byte("#!/bin/sh\n"))
	require.ErrorContains(t, verifyArtifacts(artifacts, emittedFiles, dir), `contains "stowaway.sh"`)
}

func TestVerifyPreparedRequiresMaterializedInputs(t *testing.T) {
	dir := t.TempDir()
	prepared := filepath.Join(dir, "prepared")

	require.ErrorContains(t, verifyPrepared(prepared), "unusable")
	require.NoError(t, os.Mkdir(prepared, 0700))
	require.ErrorContains(t, verifyPrepared(prepared), "wrote nothing")
	writeFile(t, filepath.Join(prepared, "runnable", "handler.py"), []byte("x\n"))
	require.NoError(t, verifyPrepared(prepared))
}

func TestResetOutputOnlyTouchesTheCLIOwnedBuildRoot(t *testing.T) {
	dir := t.TempDir()
	workspace := &resources.Workspace{Name: "proof"}
	workspace.WithDir(dir)

	owned := filepath.Join(dir, ".codefly", "build", "runnables", "backend", "word-count", "0.0.1")
	writeFile(t, filepath.Join(owned, "artifacts", "archive.tar.gz"), []byte("stale"))
	require.NoError(t, ResetOutput(workspace, owned))
	require.NoDirExists(t, owned)
	// Removing an already-absent owned directory is how a first build starts.
	require.NoError(t, ResetOutput(workspace, owned))

	// Anything outside the owned root is refused and left untouched: an
	// explicit --output can never be deleted through here.
	for _, outside := range []string{
		dir,
		filepath.Join(dir, "runnables"),
		filepath.Join(dir, ".codefly", "build"),
		filepath.Join(dir, ".codefly", "build", "runnables", ".."),
		filepath.Dir(dir),
	} {
		writeFile(t, filepath.Join(outside, "keep.txt"), []byte("keep"))
		require.ErrorContains(t, ResetOutput(workspace, outside), "not inside the workspace Runnable build root")
		require.FileExists(t, filepath.Join(outside, "keep.txt"))
	}
}

func TestDefaultOutputIsInsideTheOwnedBuildRoot(t *testing.T) {
	dir := t.TempDir()
	workspace := &resources.Workspace{Name: "proof"}
	workspace.WithDir(dir)
	r := &resources.Runnable{Name: "word-count", Version: "0.0.1"}
	r.SetModule("backend")

	output, err := DefaultOutput(workspace, r)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(dir, ".codefly", "build", "runnables", "backend", "word-count", "0.0.1"), output)
	require.NoError(t, ResetOutput(workspace, output))
}

// A rollback that removed nothing must not be reported as a completed
// rollback: the module would keep a reference to source Create then deletes.
func TestRemoveRunnableReferenceRequiresExactlyOneRemoval(t *testing.T) {
	module := &resources.Module{Name: "backend", RunnableReferences: []*resources.RunnableReference{
		{Name: "word-count"}, {Name: "other"},
	}}
	require.NoError(t, removeRunnableReference(module, "word-count"))
	require.Len(t, module.RunnableReferences, 1)
	require.Equal(t, "other", module.RunnableReferences[0].Name)

	require.ErrorContains(t, removeRunnableReference(module, "word-count"), "carries no reference")

	duplicated := &resources.Module{Name: "backend", RunnableReferences: []*resources.RunnableReference{
		{Name: "word-count"}, {Name: "word-count"},
	}}
	require.ErrorContains(t, removeRunnableReference(duplicated, "word-count"), "carried 2 references")
}
