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

// documentSuffix ends every generated document name. contentQualifierLength is
// how much of a document's checksum distinguishes two differing inventories of
// one image.
const (
	documentSuffix         = ".cdx.json"
	contentQualifierLength = 12
)

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
func Documents(evidence []*builderv0.ImageSBOM) ([]Document, error) {
	encoded := make([]Document, 0, len(evidence))
	for _, image := range evidence {
		payload, err := coresbom.MarshalCycloneDXJSON(image.GetBom())
		if err != nil {
			return nil, fmt.Errorf("encode CycloneDX for %s: %w", image.GetDigest(), err)
		}
		payload = append(payload, '\n')
		encoded = append(encoded, Document{
			Name:         Filename(image),
			Payload:      payload,
			Digest:       image.GetDigest(),
			Platform:     image.GetPlatform(),
			SHA256:       "sha256:" + resources.Hash(payload),
			Associations: Associations(image),
		})
	}
	return collapse(encoded), nil
}

// collapse merges evidence that is the same scan and keeps apart evidence that
// is not.
//
// One digest can be scanned independently by several agents — a shared base
// image, or a migration image one service builds and another deploys — and
// their inventories can genuinely differ, by scanner version or by what each
// was able to see. Collapsing those onto one document would publish one
// service's inventory as another service's coverage, which claims coverage that
// was never established. Only byte-identical documents therefore merge their
// associations; divergent ones are all kept, under names qualified by content
// so neither overwrites the other.
//
// Divergence is not an error: two honest scans of one image differ in their
// CycloneDX serial number alone, so failing here would reject the very case
// that sharing a digest across services exists to describe.
func collapse(encoded []Document) []Document {
	var documents []Document
	at := map[string]int{}
	variants := map[string]int{}
	for _, document := range encoded {
		key := document.Name + "\x00" + document.SHA256
		if index, seen := at[key]; seen {
			documents[index].Associations = mergeAssociations(documents[index].Associations, document.Associations)
			continue
		}
		at[key] = len(documents)
		variants[document.Name]++
		documents = append(documents, document)
	}
	for index := range documents {
		if variants[documents[index].Name] > 1 {
			documents[index].Name = qualify(documents[index].Name, documents[index].SHA256)
		}
	}
	return documents
}

// qualify appends a document's own checksum to its name, so two differing
// inventories of one image are both retrievable. It depends only on content,
// never on the order the scans arrived in, so a build publishes the same names
// every time.
func qualify(name, checksum string) string {
	short := strings.TrimPrefix(checksum, "sha256:")
	if len(short) > contentQualifierLength {
		short = short[:contentQualifierLength]
	}
	return strings.TrimSuffix(name, documentSuffix) + "--" + short + documentSuffix
}

// Filename names a document by the digest and platform actually scanned — the
// scan identity — so multi-image and multi-platform outputs never collide.
func Filename(image *builderv0.ImageSBOM) string {
	name := SafeName(strings.TrimPrefix(image.GetDigest(), "sha256:"))
	if platform := image.GetPlatform(); platform != "" {
		name += "--" + SafeName(platform)
	}
	return name + documentSuffix
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
