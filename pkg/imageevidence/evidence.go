// Package imageevidence owns the on-disk shape of digest-bound image SBOM
// evidence: how a scanned image becomes one CycloneDX document, what that
// document is called, and what a consumer needs recorded beside it.
//
// It exists because more than one command persists this evidence — the CI
// report and the push-bearing build — and a document written under two
// different names, or deduplicated two different ways, is evidence a consumer
// cannot match back to an image.
package imageevidence

import (
	"fmt"
	"path/filepath"
	"strings"

	coresbom "github.com/codefly-dev/core/agents/services/sbom"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
)

// MediaType is the CycloneDX JSON media type every document is recorded under.
const MediaType = "application/vnd.cyclonedx+json"

// Association names one service-owned image that a single scan covers, in the
// role the image plays for that service.
type Association struct {
	Service   string `json:"service"`
	Role      string `json:"role,omitempty"`
	Reference string `json:"reference,omitempty"`
}

// Document is one encoded CycloneDX inventory together with the scan identity
// it is bound to. Payload is what gets written; the rest is what a report or
// index records so the document can be matched back to an image.
type Document struct {
	Name         string
	Payload      []byte
	Digest       string
	Platform     string
	SHA256       string
	Associations []Association
}

// Documents encodes every scanned image into a named CycloneDX document.
//
// All documents are encoded before any is returned, so a caller writing them in
// order cannot leave the earlier images of a failed run stranded on disk.
//
// Images sharing a scan identity collapse onto one document carrying both sets
// of associations: the same digest scanned for several services is stored once,
// and merging rather than overwriting is what keeps every service's claim on it.
func Documents(evidence []*builderv0.ImageSBOM) ([]Document, error) {
	documents := make([]Document, 0, len(evidence))
	at := map[string]int{}
	for _, image := range evidence {
		payload, err := coresbom.MarshalCycloneDXJSON(image.GetBom())
		if err != nil {
			return nil, fmt.Errorf("encode CycloneDX for %s: %w", image.GetDigest(), err)
		}
		payload = append(payload, '\n')
		name := Filename(image)
		if index, seen := at[name]; seen {
			documents[index].Associations = mergeAssociations(documents[index].Associations, Associations(image))
			continue
		}
		at[name] = len(documents)
		documents = append(documents, Document{
			Name:         name,
			Payload:      payload,
			Digest:       image.GetDigest(),
			Platform:     image.GetPlatform(),
			SHA256:       "sha256:" + resources.Hash(payload),
			Associations: Associations(image),
		})
	}
	return documents, nil
}

// Filename names a document by the digest and platform actually scanned — the
// scan identity — so multi-image and multi-platform outputs never collide.
func Filename(image *builderv0.ImageSBOM) string {
	name := SafeName(strings.TrimPrefix(image.GetDigest(), "sha256:"))
	if platform := image.GetPlatform(); platform != "" {
		name += "--" + SafeName(platform)
	}
	return name + ".cdx.json"
}

// Associations projects the service subjects one scan covers.
func Associations(image *builderv0.ImageSBOM) []Association {
	associations := make([]Association, 0, len(image.GetSubjects()))
	for _, subject := range image.GetSubjects() {
		associations = append(associations, Association{
			Service:   subject.GetService(),
			Role:      subject.GetRole(),
			Reference: subject.GetReference(),
		})
	}
	return associations
}

// SafeName keeps a scan identity inside a single path segment: a platform reads
// linux/amd64 and a reference carries slashes, neither of which may escape the
// evidence directory or silently become a subdirectory.
func SafeName(value string) string {
	value = strings.ReplaceAll(value, "/", "-")
	value = strings.ReplaceAll(value, string(filepath.Separator), "-")
	value = strings.ReplaceAll(value, "..", "-")
	return value
}

func mergeAssociations(existing, incoming []Association) []Association {
	for _, candidate := range incoming {
		duplicate := false
		for _, present := range existing {
			if present == candidate {
				duplicate = true
				break
			}
		}
		if !duplicate {
			existing = append(existing, candidate)
		}
	}
	return existing
}
