package publish

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/google/go-github/v89/github"
)

// stageReleaseChecksums supplies the same producer evidence as workflow-owned
// publication. Upload archives first and their complete checksum list last;
// retries use the same sorted bytes and never replace an existing asset.
func stageReleaseChecksums(assets []loaderAsset) ([]string, error) {
	var files []string
	for _, asset := range assets {
		files = append(files, asset.archivePath)
		if asset.sbomPath != "" {
			files = append(files, asset.sbomPath)
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("a release publishes no assets to checksum")
	}
	sort.Slice(files, func(i, j int) bool { return filepath.Base(files[i]) < filepath.Base(files[j]) })
	var checksums strings.Builder
	for i, file := range files {
		name := filepath.Base(file)
		if name == "checksums.txt" || (i > 0 && name == filepath.Base(files[i-1])) {
			return nil, fmt.Errorf("duplicate release asset name %q", name)
		}
		digest, err := releaseFileDigest(file)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(&checksums, "%s  %s\n", digest, name)
	}
	target := filepath.Join(filepath.Dir(files[0]), "checksums.txt")
	if err := os.WriteFile(target, []byte(checksums.String()), 0o600); err != nil {
		return nil, err
	}
	return append(files, target), nil
}

func releaseFileDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", digest.Sum(nil)), nil
}

func validateExistingReleaseAsset(path string, existing map[string]*github.ReleaseAsset) error {
	asset, exists := existing[filepath.Base(path)]
	if !exists {
		return nil
	}
	digest, err := releaseFileDigest(path)
	if err != nil {
		return err
	}
	if asset.GetDigest() != "sha256:"+digest {
		return fmt.Errorf("release asset %s already exists with different or unverifiable bytes; published assets cannot be replaced", filepath.Base(path))
	}
	return nil
}
