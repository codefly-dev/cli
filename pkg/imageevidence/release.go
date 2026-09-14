package imageevidence

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// IndexFilename is the manifest a consumer reads to find evidence for an image.
const IndexFilename = "index.json"

// indexVersion is the shape of the manifest, not of the evidence it points at.
const indexVersion = 1

// Index is the manifest written beside the documents. A pushed image is
// identified by its digest, so a consumer holding an image reference resolves
// its evidence by matching the digest here rather than by guessing a filename.
type Index struct {
	Version int         `json:"version"`
	Images  []IndexItem `json:"images"`
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
func Publish(directory string, documents []Document) (Index, error) {
	index := Index{Version: indexVersion, Images: make([]IndexItem, 0, len(documents))}
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
	return index, nil
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
