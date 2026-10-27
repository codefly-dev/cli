package publish

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStageReleaseChecksumsIsDeterministicAndComplete(t *testing.T) {
	assets := stageAssets(t, "z.tar.gz", "a.tar.gz")
	assets[0].sbomPath = stageFile(t, filepath.Dir(assets[0].archivePath), "z.cdx.json")
	files, err := stageReleaseChecksums(assets)
	require.NoError(t, err)
	var names []string
	for _, file := range files {
		names = append(names, filepath.Base(file))
	}
	require.Equal(t, []string{"a.tar.gz", "z.cdx.json", "z.tar.gz", "checksums.txt"}, names)
	var expected string
	for _, name := range names[:3] {
		expected += fmt.Sprintf("%x  %s\n", sha256.Sum256([]byte("payload-"+name)), name)
	}
	raw, err := os.ReadFile(files[3])
	require.NoError(t, err)
	require.Equal(t, expected, string(raw))
	_, err = stageReleaseChecksums([]loaderAsset{assets[1], assets[0]})
	require.NoError(t, err)
	retried, err := os.ReadFile(files[3])
	require.NoError(t, err)
	require.Equal(t, raw, retried)
}

func TestStageReleaseChecksumsRefusesAmbiguousNames(t *testing.T) {
	for _, names := range [][]string{{"same.tar.gz", "same.tar.gz"}, {"checksums.txt"}} {
		_, err := stageReleaseChecksums(stageAssets(t, names...))
		require.ErrorContains(t, err, "duplicate release asset name")
	}
}

func TestReleaseRetryPreflightsEveryImmutableAssetBeforeMutation(t *testing.T) {
	const tag = "v0.0.16"
	for _, scenario := range []string{"complete identical retry", "late archive conflict", "checksum conflict"} {
		t.Run(scenario, func(t *testing.T) {
			assets := stageAssets(t, "a.tar.gz", "z.tar.gz")
			files, err := stageReleaseChecksums(assets)
			require.NoError(t, err)
			existing := map[string]int64{}
			f := &fakeGitHub{digests: map[string]string{}}
			for i, file := range files {
				raw, err := os.ReadFile(file)
				require.NoError(t, err)
				existing[filepath.Base(file)] = int64(i + 1)
				f.digests[filepath.Base(file)] = fmt.Sprintf("sha256:%x", sha256.Sum256(raw))
			}
			switch scenario {
			case "late archive conflict":
				delete(existing, "a.tar.gz") // would upload before discovering z's conflict
				require.NoError(t, os.WriteFile(assets[1].archivePath, []byte("different"), 0600))
			case "checksum conflict":
				delete(existing, "a.tar.gz")
				f.digests["checksums.txt"] = digestOf("old evidence")
			}
			client := f.server(t, tag, existing)
			err = createAndUploadRelease(t.Context(), client, "codefly-dev", "service-go", tag, assets)
			if scenario == "complete identical retry" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "cannot be replaced")
			}
			require.False(t, f.created)
			require.Empty(t, f.uploaded)
			require.Empty(t, f.deleted)
		})
	}
}

func TestVerifyPublishedArchivesChecksExplicitSBOM(t *testing.T) {
	const sbom = "service-go.cdx.json"
	for _, scenario := range []string{"present", "missing", "wrong bytes", "wrong producer digest"} {
		t.Run(scenario, func(t *testing.T) {
			p := publishedBy(t, map[string]string{testArchive: "archive bytes", sbom: "sbom bytes"})
			switch scenario {
			case "missing":
				delete(p.payloads, sbom)
			case "wrong bytes":
				p.payloads[sbom] = "different sbom"
			case "wrong producer digest":
				p.checksumDigests = map[string]string{sbom: fmt.Sprintf("%x", sha256.Sum256([]byte("old sbom")))}
			}
			err := p.withChecksums().verify([]loaderAsset{{archivePath: testArchive, sbomPath: sbom}})
			if scenario == "present" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, sbom)
			}
		})
	}
}
