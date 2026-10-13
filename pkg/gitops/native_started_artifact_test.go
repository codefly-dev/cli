package gitops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/codefly-dev/core/agents/contract"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/stretchr/testify/require"
)

// onPythonAgent moves a fixture service off the Go family. The whole point of
// these cases is a service whose agent lays its workload out some other way,
// so the replacement is asserted rather than assumed: a fixture that quietly
// stopped naming go-grpc would otherwise leave every case below testing Go.
func onPythonAgent(t *testing.T, service string) string {
	t.Helper()
	const goAgent = "name: go-grpc\n  version: 0.0.1\n  publisher: codefly.ai"
	require.Contains(t, service, goAgent, "the shared service fixture no longer names the Go agent")
	return strings.Replace(service, goAgent, "name: python-fastapi\n  version: 0.0.112\n  publisher: codefly.dev", 1)
}

// startedPythonService lays down what a python agent starts: the interpreter
// it ran, and the sources it handed that interpreter.
func startedPythonService(t *testing.T, dir string) NativeArtifact {
	t.Helper()
	files := map[string]string{
		filepath.Join(".venv", "bin", "python"): "cpython 3.13",
		filepath.Join("src", "main.py"):         "from fastapi import FastAPI\napp = FastAPI()\n",
		filepath.Join("src", "routes.py"):       "def health():\n    return {}\n",
		"uv.lock":                               "version = 1\n",
	}
	for relative, content := range files {
		full := filepath.Join(dir, relative)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o600))
	}
	return NativeArtifact{
		Executable: filepath.Join(dir, ".venv", "bin", "python"),
		Sources: []string{
			filepath.Join(dir, "src", "main.py"),
			filepath.Join(dir, "src", "routes.py"),
			filepath.Join(dir, "uv.lock"),
		},
	}
}

// A compiled runtime starts its own build, so the build stays the SHA-256 of
// that one executable — the same value a Go workload was approved under before
// the agent reported anything, so adopting the contract does not re-approve
// every build in the host's envelope.
func TestTheBuildOfACompiledRuntimeIsTheExecutableItStarted(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "main")
	require.NoError(t, os.WriteFile(path, []byte("one build"), 0o700))

	first, err := NativeArtifact{Executable: path}.Digest(root)
	require.NoError(t, err)
	require.Equal(t, solutionhost.ImageDigest("sha256:35b5c9836731bd31e75d30f3186350b17b62adf380da9d852d822b741f05922a"), first)

	require.NoError(t, os.WriteFile(path, []byte("another build"), 0o700))
	second, err := NativeArtifact{Executable: path}.Digest(root)
	require.NoError(t, err)
	require.NotEqual(t, first, second, "a rebuild is a different build")

	_, err = NativeArtifact{Executable: filepath.Join(t.TempDir(), "absent")}.Digest(root)
	require.Error(t, err)

	_, err = NativeArtifact{}.Digest(root)
	require.ErrorContains(t, err, "no started executable")
}

// An interpreted runtime starts an interpreter, which says nothing about the
// service: the build is the closure the agent named, and every file in it is
// part of what the host approves.
func TestTheBuildOfAnInterpretedRuntimeIsTheClosureTheAgentReported(t *testing.T) {
	root := t.TempDir()
	artifact := startedPythonService(t, root)

	digest, err := artifact.Digest(root)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(string(digest), "sha256:"))

	interpreter, err := NativeArtifact{Executable: artifact.Executable}.Digest(root)
	require.NoError(t, err)
	require.NotEqual(t, interpreter, digest, "the interpreter alone is not the build: every python service would share one")

	// The agent's enumeration order is not part of the build, and naming a
	// file twice is not a different build either.
	shuffled := NativeArtifact{Executable: artifact.Executable, Sources: []string{
		artifact.Sources[2], artifact.Sources[1], artifact.Sources[0], artifact.Sources[1],
	}}
	same, err := shuffled.Digest(root)
	require.NoError(t, err)
	require.Equal(t, digest, same)

	// Editing any source is a different build — including the dependency lock,
	// which is as much what runs as the code importing it.
	for _, source := range artifact.Sources {
		original, readErr := os.ReadFile(source) //nolint:gosec // G304: a path this test wrote under its own TempDir
		require.NoError(t, readErr)
		require.NoError(t, os.WriteFile(source, append(original, []byte("# edited\n")...), 0o600))
		edited, digestErr := artifact.Digest(root)
		require.NoError(t, digestErr)
		require.NotEqual(t, digest, edited, "%s changed and the build did not", source)
		require.NoError(t, os.WriteFile(source, original, 0o600))
	}
	restored, err := artifact.Digest(root)
	require.NoError(t, err)
	require.Equal(t, digest, restored, "the same tree is the same build")

	// A file the agent names is a file: a directory is the agent declining to
	// say what its closure contains, which is not the run's to decide.
	_, err = NativeArtifact{Executable: artifact.Executable, Sources: []string{filepath.Join(root, "src")}}.Digest(root)
	require.ErrorContains(t, err, "not a regular file")
}

// The closure is named relative to the workspace, so the same checkout in a
// different directory is the same build and the host's envelope does not grow
// a second approval for it.
func TestAClosureDigestDoesNotMoveWithTheCheckout(t *testing.T) {
	first := t.TempDir()
	here, err := startedPythonService(t, first).Digest(first)
	require.NoError(t, err)

	second := t.TempDir()
	there, err := startedPythonService(t, second).Digest(second)
	require.NoError(t, err)
	require.Equal(t, here, there)

	// One tree reached by two spellings of the same directory is one build.
	// This is the macOS case: the workspace is known as /var/folders/... and
	// the agent reports /private/var/folders/..., and relating those two
	// lexically names every file absolutely instead of relative to the
	// workspace — the same checkout, two builds. The root and the reported
	// paths are deliberately given in DIFFERENT spellings here.
	link := filepath.Join(t.TempDir(), "workspace")
	require.NoError(t, os.Symlink(second, link))
	artifact := startedPythonService(t, second)
	through := NativeArtifact{Executable: strings.Replace(artifact.Executable, second, link, 1)}
	for _, source := range artifact.Sources {
		through.Sources = append(through.Sources, strings.Replace(source, second, link, 1))
	}
	linked, err := through.Digest(second)
	require.NoError(t, err)
	require.Equal(t, there, linked, "the agent's spelling of the workspace changed the build")

	// And the other way round: the root spelled through the link, the files
	// reported by their real paths.
	other, err := startedPythonService(t, second).Digest(link)
	require.NoError(t, err)
	require.Equal(t, there, other, "the run's spelling of the workspace changed the build")
}

// A virtualenv's interpreter is a symlink to a file outside the workspace,
// and that is the normal python layout — `.venv/bin/python` points at
// whatever interpreter uv or pyenv installed. The workspace holds the link at
// a stable name, so the build must be named after the link. Following it
// would make a homebrew revision bump, a pyenv rehash or simply another
// developer's machine a new build for a tree nobody changed, which is the
// spurious approval the workspace-relative naming exists to avoid.
func TestAVirtualenvInterpreterIsNamedWhereTheWorkspaceHoldsIt(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "main.py")
	require.NoError(t, os.WriteFile(source, []byte("app = None\n"), 0o600))
	venv := filepath.Join(root, ".venv", "bin")
	require.NoError(t, os.MkdirAll(venv, 0o755))
	link := filepath.Join(venv, "python")
	artifact := NativeArtifact{Executable: link, Sources: []string{source}}

	// Two byte-identical interpreters, installed at different absolute paths.
	digests := make([]solutionhost.ImageDigest, 0, 2)
	for range 2 {
		interpreter := filepath.Join(t.TempDir(), "python3.13")
		require.NoError(t, os.WriteFile(interpreter, []byte("cpython 3.13"), 0o700))
		require.NoError(t, os.RemoveAll(link))
		require.NoError(t, os.Symlink(interpreter, link))
		digest, err := artifact.Digest(root)
		require.NoError(t, err)
		digests = append(digests, digest)
	}
	require.Equal(t, digests[0], digests[1],
		"the interpreter moved and its bytes did not, so the build must not have moved either")

	// The content is still read through the link: a different interpreter is
	// a different build.
	other := filepath.Join(t.TempDir(), "python3.14")
	require.NoError(t, os.WriteFile(other, []byte("cpython 3.14"), 0o700))
	require.NoError(t, os.RemoveAll(link))
	require.NoError(t, os.Symlink(other, link))
	upgraded, err := artifact.Digest(root)
	require.NoError(t, err)
	require.NotEqual(t, digests[0], upgraded, "the interpreter changed and the build did not")
}

// A file the closure needs but the workspace does not hold — the interpreter
// in /usr, a shared virtualenv — has only its absolute path for a name, and
// that path is part of the build. Nothing may silently name it relative to
// the workspace, which would make the build depend on how deep the checkout
// sits on disk.
func TestAFileOutsideTheWorkspaceIsNamedByItsAbsolutePath(t *testing.T) {
	outside := t.TempDir()
	interpreter := filepath.Join(outside, "python")
	require.NoError(t, os.WriteFile(interpreter, []byte("cpython"), 0o700))

	shallow := t.TempDir()
	deep := filepath.Join(t.TempDir(), "a", "b", "c")
	require.NoError(t, os.MkdirAll(deep, 0o755))

	digests := map[string]solutionhost.ImageDigest{}
	for _, root := range []string{shallow, deep} {
		source := filepath.Join(root, "main.py")
		require.NoError(t, os.WriteFile(source, []byte("app = None\n"), 0o600))
		digest, err := NativeArtifact{Executable: interpreter, Sources: []string{source}}.Digest(root)
		require.NoError(t, err)
		digests[root] = digest
	}
	require.Equal(t, digests[shallow], digests[deep],
		"the interpreter is named absolutely in both, so only the workspace-relative source decides the build")

	// And the absolute name is really in the digest: a different interpreter
	// at a different path is a different build even with identical bytes.
	twin := filepath.Join(t.TempDir(), "python")
	require.NoError(t, os.WriteFile(twin, []byte("cpython"), 0o700))
	source := filepath.Join(shallow, "main.py")
	moved, err := NativeArtifact{Executable: twin, Sources: []string{source}}.Digest(shallow)
	require.NoError(t, err)
	require.NotEqual(t, digests[shallow], moved)
}

// textManifestDigest is the manifest encoding this file used before it was
// length-prefixed: one "<sha256>  <name>" line per entry, sorted. It lives
// here so the case below can show what the prefix is for — a name is free to
// contain a newline, and under this encoding one file could spell out two
// entries.
func textManifestDigest(t *testing.T, root string, artifact NativeArtifact) solutionhost.ImageDigest {
	t.Helper()
	root = resolved(root)
	var lines []string
	for _, path := range append([]string{artifact.Executable}, artifact.Sources...) {
		digest, err := fileDigest(path)
		require.NoError(t, err)
		lines = append(lines, string(digest)+"  "+artifactName(root, path)+"\n")
	}
	sort.Strings(lines)
	hash := sha256.New()
	for _, line := range lines {
		hash.Write([]byte(line))
	}
	return solutionhost.ImageDigest("sha256:" + hex.EncodeToString(hash.Sum(nil)))
}

// A source's name cannot encode entries of its own. One file named
// "a\n<sha256>  g" renders, in a plain text manifest, as exactly the two lines
// an honest two-file closure renders — so two different started closures
// would share one approved build, and the host would admit either of them for
// the other. The manifest is length-prefixed, which is what makes the name
// unambiguous.
func TestASourceNameCannotForgeAManifestEntry(t *testing.T) {
	root := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(root, name)
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
		return path
	}
	digestOf := func(content string) string {
		sum := sha256.Sum256([]byte(content))
		return "sha256:" + hex.EncodeToString(sum[:])
	}

	// Entries sort by the whole string, which starts with a fixed-width
	// digest, so the forgery only merges two adjacent lines when the two
	// honest digests are neighbours in that order. Search content for a
	// layout where they are.
	const executableContent = "cpython"
	executableDigest := digestOf(executableContent)
	var aContent, gContent string
	for i := 0; aContent == "" && i < 512; i++ {
		for j := 0; j < 512; j++ {
			a, g := fmt.Sprintf("a%d\n", i), fmt.Sprintf("g%d\n", j)
			da, dg := digestOf(a), digestOf(g)
			if (executableDigest < da && da < dg) || (da < dg && dg < executableDigest) {
				aContent, gContent = a, g
				break
			}
		}
	}
	require.NotEmpty(t, aContent, "no content layout puts the two honest entries side by side")

	executable := write("python", executableContent)
	honest := NativeArtifact{Executable: executable, Sources: []string{
		write("a", aContent), write("g", gContent),
	}}
	// One file, named so that a text manifest renders it as those two lines.
	forged := NativeArtifact{Executable: executable, Sources: []string{
		write("a\n"+digestOf(gContent)+"  g", aContent),
	}}

	require.Equal(t, textManifestDigest(t, root, honest), textManifestDigest(t, root, forged),
		"the text manifest this replaced gave two different closures one build")

	honestBuild, err := honest.Digest(root)
	require.NoError(t, err)
	forgedBuild, err := forged.Digest(root)
	require.NoError(t, err)
	require.NotEqual(t, honestBuild, forgedBuild, "a name forged a manifest entry")

	// The two digest shapes must stay apart. A compiled runtime's build is
	// the sha256 of its executable's own bytes and a closure's is the sha256
	// of a manifest, so an executable whose bytes ARE that manifest would
	// otherwise carry the closure's approved build. The manifest is opened
	// with a domain tag, which the preimage written here deliberately omits.
	preimage := write("preimage", closureManifestEntries(t, root, honest))
	impostor, err := NativeArtifact{Executable: preimage}.Digest(root)
	require.NoError(t, err)
	require.NotEqual(t, honestBuild, impostor, "an executable whose bytes are a manifest took that closure's build")
}

// closureManifestEntries is the entry stream Digest hashes for a closure,
// without the domain tag that opens it — the preimage an executable's bytes
// would have to be to be read as that closure.
func closureManifestEntries(t *testing.T, root string, artifact NativeArtifact) string {
	t.Helper()
	root = resolved(root)
	var entries []string
	for _, path := range append([]string{artifact.Executable}, artifact.Sources...) {
		digest, err := fileDigest(path)
		require.NoError(t, err)
		entries = append(entries, manifestEntry(artifactName(root, path), digest))
	}
	sort.Strings(entries)
	return strings.Join(entries, "")
}

// An agent that names the binary it ran among its sources has described the
// same process as one that leaves the list empty. Both must be one build, or
// a compiled runtime's build depends on how its agent spells the report.
func TestNamingTheExecutableAmongTheSourcesIsTheSameBuild(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "main")
	require.NoError(t, os.WriteFile(binary, []byte("one build"), 0o700))

	alone, err := NativeArtifact{Executable: binary}.Digest(root)
	require.NoError(t, err)
	listed, err := NativeArtifact{Executable: binary, Sources: []string{binary}}.Digest(root)
	require.NoError(t, err)
	require.Equal(t, alone, listed)

	// It is dropped, not ignored: a real closure beside it still digests as a
	// closure.
	source := filepath.Join(root, "main.py")
	require.NoError(t, os.WriteFile(source, []byte("app = None\n"), 0o600))
	closure, err := NativeArtifact{Executable: binary, Sources: []string{binary, source}}.Digest(root)
	require.NoError(t, err)
	require.NotEqual(t, alone, closure)
	withoutSelf, err := NativeArtifact{Executable: binary, Sources: []string{source}}.Digest(root)
	require.NoError(t, err)
	require.Equal(t, closure, withoutSelf)
}

// An agent that reports nothing is where a native run stops. It must say which
// agent that is: adopting the contract there is the only thing that lifts the
// refusal, and the old message named a Go binary path the service never had.
func TestAnUnreportedArtifactIsRefusedByNamingTheAgent(t *testing.T) {
	workspace, _ := writeAuthorityWorkspaceRunning(t, onPythonAgent(t, exposedHostedServiceYAML("api", "shop"))+"module-identity: true\n")
	service := loadServices(t, workspace, "shop", "api")[0]

	_, err := StartedArtifactOf(service, nil)
	require.ErrorContains(t, err, "codefly.dev/python-fastapi:0.0.112")
	require.ErrorContains(t, err, contract.RuntimeStartedArtifact)
	require.ErrorContains(t, err, "shop/api")
	require.NotContains(t, err.Error(), ".cache", "the refusal must not name a path from one agent family's layout")

	// An artifact naming no program names nothing, however many sources it
	// lists: the same refusal, not an empty digest.
	_, err = StartedArtifactOf(service, &runtimev0.StartedArtifact{Sources: []string{"/workspace/src/main.py"}})
	require.ErrorContains(t, err, "codefly.dev/python-fastapi:0.0.112")

	// A relative path would be opened against this process's working
	// directory — the user's terminal — and would digest whatever file
	// happened to sit there. The contract says absolute; a report that is not
	// is refused rather than resolved.
	_, err = StartedArtifactOf(service, &runtimev0.StartedArtifact{Executable: "go.mod"})
	require.ErrorContains(t, err, "not an absolute path")
	require.ErrorContains(t, err, "codefly.dev/python-fastapi:0.0.112")
	_, err = StartedArtifactOf(service, &runtimev0.StartedArtifact{
		Executable: "/opt/venv/bin/python", Sources: []string{"src/main.py"},
	})
	require.ErrorContains(t, err, "not an absolute path")

	reported, err := StartedArtifactOf(service, &runtimev0.StartedArtifact{
		Executable: "/opt/venv/bin/python", Sources: []string{"/workspace/src/main.py"},
	})
	require.NoError(t, err)
	require.Equal(t, NativeArtifact{Executable: "/opt/venv/bin/python", Sources: []string{"/workspace/src/main.py"}}, reported)
}

// The delivery of a module-identity service on a non-Go agent: its presence
// names the build the agent reported starting, and its authority approves that
// same build. Nothing reads a path of the run's own choosing — the service has
// no `.cache/native/main`, which is exactly what used to stop it being
// delivered at all (#989).
func TestANativeRunDeliversAServiceOnANonGoAgent(t *testing.T) {
	ctx := context.Background()
	workspace, module := writeAuthorityWorkspaceRunning(t, onPythonAgent(t, exposedHostedServiceYAML("api", "shop"))+"module-identity: true\n")
	env := selectedEnvironment(t, workspace, "staging")
	services := loadServices(t, workspace, "shop", "api")

	artifact := startedPythonService(t, services[0].Dir())
	require.NoFileExists(t, filepath.Join(services[0].Dir(), ".cache", "native", "main"))

	deliverer := &NativeDeliverer{Workspace: workspace, Env: env, StateDir: t.TempDir()}
	documents, err := deliverer.Render(ctx, &NativeWorkload{
		Module: module, Service: services[0], Services: services,
		Namespace: "acme-shop", Account: "api", SPIFFEID: "spiffe://cluster.example/ns/acme-shop/sa/api",
		Subject: "shop-api@example.iam.test", Artifact: artifact,
	})
	require.NoError(t, err)

	expected, err := artifact.Digest(workspace.Dir())
	require.NoError(t, err)
	require.Equal(t, expected, documents.Digest)
	require.Len(t, documents.Presence.Workloads, 1)
	require.Equal(t, expected, documents.Presence.Workloads[0].Image.Digest)
	require.Equal(t, "native/acme/shop/api", documents.Presence.Workloads[0].Image.Repository)
	require.NotNil(t, documents.Authority)
	require.Equal(t, expected, documents.Authority.ApprovedBuild)

	// A rebuilt source is a new build the host must approve afresh, the same
	// way a recompiled Go binary is.
	require.NoError(t, os.WriteFile(artifact.Sources[0], []byte("app = None\n"), 0o600))
	rebuilt, err := deliverer.Render(ctx, &NativeWorkload{
		Module: module, Service: services[0], Services: services,
		Namespace: "acme-shop", Account: "api", SPIFFEID: "spiffe://cluster.example/ns/acme-shop/sa/api",
		Subject: "shop-api@example.iam.test", Artifact: artifact,
	})
	require.NoError(t, err)
	require.NotEqual(t, documents.Digest, rebuilt.Digest)
}

// The forgery case above pins one historical attack. This pins the property
// behind it: whatever a file is called, one entry decodes back to exactly the
// name and digest it was written from, so no name can be read as a name and a
// digest belonging to some other file. A format change that keeps the earlier
// case green but loses this — dropping the space after the length, say — is a
// format two closures can again share a build under.
func TestAManifestEntryDecodesBackToTheFileItNamed(t *testing.T) {
	digest := solutionhost.ImageDigest("sha256:" + strings.Repeat("ab", 32))
	names := []string{
		"main.py",
		"src/a b.py",                         // a space, where a delimiter would be
		"a\n" + string(digest) + "  g",       // the forgery: a whole entry in a name
		"12 main.py",                         // leads with digits and a space, like a length prefix
		"",                                   // empty, which a count still states
		"módulo/\t.py",                       // multi-byte and a tab: a length is bytes, not runes
		strings.Repeat("deep/", 40) + "x.py", // long enough that the count is multi-digit
	}
	for _, name := range names {
		entry := manifestEntry(name, digest)

		// Decode exactly as the encoding promises: a decimal count, one
		// space, that many BYTES of name, one space, the digest, a newline.
		head, rest, found := strings.Cut(entry, " ")
		require.True(t, found, "entry for %q has no length prefix", name)
		length, err := strconv.Atoi(head)
		require.NoError(t, err, "entry for %q does not open with a byte count", name)
		require.LessOrEqual(t, length, len(rest))
		decodedName, tail := rest[:length], rest[length:]
		require.Equal(t, name, decodedName)
		require.Equal(t, " "+string(digest)+"\n", tail, "entry for %q does not end in exactly its digest", name)
	}

	// And two different names never write the same entry, which is what
	// "one closure, one digest" rests on.
	seen := map[string]string{}
	for _, name := range names {
		entry := manifestEntry(name, digest)
		previous, collided := seen[entry]
		require.False(t, collided, "%q and %q write the same manifest entry", previous, name)
		seen[entry] = name
	}
}
