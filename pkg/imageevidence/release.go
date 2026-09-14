package imageevidence

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

// IndexFilename is the manifest a consumer reads to find evidence for an image.
const IndexFilename = "index.json"

// indexVersion is the shape of the manifest, not of the evidence it points at.
const indexVersion = 1

// Index is the manifest written beside the documents. A pushed image is
// identified by its digest, so a consumer holding an image reference resolves
// its evidence by matching the digest here rather than by guessing a filename.
//
// RegistryBacked records whether the digests below were resolved from a push.
// A build that does not push scans an image that exists only in the local
// daemon, whose digest resolves in no registry; without this the two are
// indistinguishable and local evidence reads as evidence for a shipped image.
type Index struct {
	Version        int         `json:"version"`
	RegistryBacked bool        `json:"registry_backed"`
	Images         []IndexItem `json:"images"`
}

// IndexItem points at one document and carries the scan identity it is bound to
// along with every service association the scan covers.
type IndexItem struct {
	Path         string        `json:"path"`
	MediaType    string        `json:"media_type"`
	SHA256       string        `json:"sha256"`
	Digest       string        `json:"digest"`
	Platform     string        `json:"platform,omitempty"`
	Associations []Association `json:"associations,omitempty"`
}

// Publish writes every document into directory together with an index naming
// them. It is the release-artifact form of image evidence: a directory that
// travels with the images a build pushed, retrievable without the report of the
// run that produced it.
//
// Items are ordered by document name so the same build publishes a
// byte-identical index, and every write is atomic so a reader never observes a
// half-written document or an index pointing at one.
//
// The order of the three steps is what keeps a failure honest. Documents are
// written first, the index second, superseded documents only once the index
// naming them is durable. A run that fails part-way therefore leaves the
// previous index and every document it names intact — an older release, still
// internally consistent — rather than a current index pointing at documents
// that were never written.
func Publish(directory string, documents []Document, registryBacked bool) (Index, error) {
	index := Index{Version: indexVersion, RegistryBacked: registryBacked, Images: make([]IndexItem, 0, len(documents))}
	for _, document := range documents {
		if err := writeAtomic(filepath.Join(directory, document.Name), document.Payload); err != nil {
			return Index{}, err
		}
		index.Images = append(index.Images, IndexItem{
			Path:         document.Name,
			MediaType:    MediaType,
			SHA256:       document.SHA256,
			Digest:       document.Digest,
			Platform:     document.Platform,
			Associations: document.Associations,
		})
	}
	sort.Slice(index.Images, func(i, j int) bool { return index.Images[i].Path < index.Images[j].Path })
	manifest, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return Index{}, fmt.Errorf("encode image evidence index: %w", err)
	}
	if err := writeAtomic(filepath.Join(directory, IndexFilename), append(manifest, '\n')); err != nil {
		return Index{}, err
	}
	if err := pruneSuperseded(directory, index); err != nil {
		return index, err
	}
	return index, nil
}

// generatedDocument matches a name Filename produced: a 64-character image
// digest, optionally followed by a platform and a content qualifier. It is what
// bounds pruning to this mechanism's own output — a source SBOM named after its
// module and service cannot match it, so sharing a directory never costs a file
// this package did not write.
var generatedDocument = regexp.MustCompile(`^[0-9a-f]{64}(--[0-9A-Za-z._-]+)*\.cdx\.json$`)

// pruneSuperseded removes documents an earlier run published that this index no
// longer names. A published directory is reused across builds, so a superseded
// document left behind lets a consumer that reads the directory rather than the
// index attribute a previous release's image to this one.
func pruneSuperseded(directory string, index Index) error {
	published := map[string]bool{}
	for _, item := range index.Images {
		published[item.Path] = true
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return fmt.Errorf("read image evidence directory: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || published[name] || !generatedDocument.MatchString(name) {
			continue
		}
		if err := os.Remove(filepath.Join(directory, name)); err != nil {
			return fmt.Errorf("remove superseded image evidence %s: %w", name, err)
		}
	}
	return nil
}

func writeAtomic(destination string, payload []byte) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return fmt.Errorf("create image evidence directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".codefly-image-evidence-*")
	if err != nil {
		return fmt.Errorf("stage %s: %w", destination, err)
	}
	path := temporary.Name()
	defer os.Remove(path)
	if err := temporary.Chmod(0o644); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("stage %s: %w", destination, err)
	}
	if _, err := temporary.Write(payload); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write %s: %w", destination, err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write %s: %w", destination, err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("write %s: %w", destination, err)
	}
	if err := os.Rename(path, destination); err != nil {
		return fmt.Errorf("publish %s: %w", destination, err)
	}
	return nil
}
