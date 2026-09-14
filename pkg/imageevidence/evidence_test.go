package imageevidence

import (
	"strings"
	"testing"

	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/stretchr/testify/require"
)

func inventory(name string) *agentv0.Bom {
	return &agentv0.Bom{
		BomFormat:   "CycloneDX",
		SpecVersion: "1.5",
		Version:     1,
		Metadata:    &agentv0.Metadata{Component: &agentv0.Component{Name: name, Version: "v1"}},
		Components:  []*agentv0.Component{{Name: "libc", Version: "2.39"}},
	}
}

func scanned(digest, platform string, subjects ...*builderv0.ImageSubject) *builderv0.ImageSBOM {
	return &builderv0.ImageSBOM{
		Digest:   digest,
		Platform: platform,
		Subjects: subjects,
		Bom:      inventory("image"),
	}
}

func TestDocumentsNameEveryImageByItsScanIdentity(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	other := "sha256:" + strings.Repeat("b", 64)

	documents, err := Documents([]*builderv0.ImageSBOM{
		scanned(digest, "linux/amd64", &builderv0.ImageSubject{Service: "web/api", Role: "app"}),
		scanned(digest, "linux/arm64", &builderv0.ImageSubject{Service: "web/api", Role: "app"}),
		scanned(other, "linux/amd64", &builderv0.ImageSubject{Service: "web/api", Role: "migration"}),
	})
	require.NoError(t, err)
	require.Len(t, documents, 3)

	names := map[string]bool{}
	for _, document := range documents {
		require.NotContains(t, document.Name, "/", "a platform separator escaped into a path")
		require.False(t, names[document.Name], "scan identities collided on %q", document.Name)
		names[document.Name] = true
	}
}

// #651 requires an identical digest to be stored once *without losing service
// associations*, so a repeated scan identity has to merge its claims rather than
// overwrite the document that already covers it.
func TestDocumentsMergeAssociationsOfARepeatedScanIdentity(t *testing.T) {
	digest := "sha256:" + strings.Repeat("c", 64)

	documents, err := Documents([]*builderv0.ImageSBOM{
		scanned(digest, "linux/amd64", &builderv0.ImageSubject{Service: "web/api", Role: "app"}),
		scanned(digest, "linux/amd64", &builderv0.ImageSubject{Service: "billing/worker", Role: "app"}),
	})
	require.NoError(t, err)
	require.Len(t, documents, 1, "one digest on one platform is one scanned image")
	require.ElementsMatch(t, []Association{
		{Service: "web/api", Role: "app"},
		{Service: "billing/worker", Role: "app"},
	}, documents[0].Associations)
}

func TestDocumentsDropARepeatedAssociationRatherThanRestatingIt(t *testing.T) {
	digest := "sha256:" + strings.Repeat("d", 64)
	subject := &builderv0.ImageSubject{Service: "web/api", Role: "app"}

	documents, err := Documents([]*builderv0.ImageSBOM{
		scanned(digest, "linux/amd64", subject),
		scanned(digest, "linux/amd64", subject),
	})
	require.NoError(t, err)
	require.Len(t, documents, 1)
	require.Len(t, documents[0].Associations, 1)
}

// A caller writes documents in order, so a document that fails to encode must
// fail the whole set: returning the ones encoded so far would let a failed run
// leave partial evidence on disk.
func TestDocumentsReturnNothingWhenOneImageCannotBeEncoded(t *testing.T) {
	digest := "sha256:" + strings.Repeat("e", 64)
	unencodable := &builderv0.ImageSBOM{Digest: "sha256:" + strings.Repeat("f", 64), Platform: "linux/amd64"}

	documents, err := Documents([]*builderv0.ImageSBOM{
		scanned(digest, "linux/amd64", &builderv0.ImageSubject{Service: "web/api"}),
		unencodable,
	})
	require.Error(t, err)
	require.Nil(t, documents)
}

func TestDocumentsBindEachPayloadToItsScanIdentityAndChecksum(t *testing.T) {
	digest := "sha256:" + strings.Repeat("1", 64)

	documents, err := Documents([]*builderv0.ImageSBOM{
		scanned(digest, "linux/amd64", &builderv0.ImageSubject{Service: "web/api", Role: "app", Reference: "repo/api:v1"}),
	})
	require.NoError(t, err)
	require.Len(t, documents, 1)

	document := documents[0]
	require.Equal(t, digest, document.Digest)
	require.Equal(t, "linux/amd64", document.Platform)
	require.True(t, strings.HasPrefix(document.SHA256, "sha256:"))
	require.Equal(t, []Association{{Service: "web/api", Role: "app", Reference: "repo/api:v1"}}, document.Associations)
	require.Contains(t, string(document.Payload), "CycloneDX")
	require.True(t, strings.HasSuffix(string(document.Payload), "\n"))
}

func TestSafeNameCannotEscapeTheEvidenceDirectory(t *testing.T) {
	require.Equal(t, "--etc-passwd", SafeName("../etc/passwd"))
}

func divergent(digest, component string, subject *builderv0.ImageSubject) *builderv0.ImageSBOM {
	image := scanned(digest, "linux/amd64", subject)
	image.Bom = &agentv0.Bom{
		BomFormat:   "CycloneDX",
		SpecVersion: "1.5",
		Version:     1,
		Metadata:    &agentv0.Metadata{Component: &agentv0.Component{Name: "image", Version: "v1"}},
		Components:  []*agentv0.Component{{Name: component, Version: "1"}},
	}
	return image
}

// Two agents can scan one digest independently and return different
// inventories. Collapsing them onto one document published one service's
// inventory as the other's coverage — a claim no scan established.
func TestDocumentsKeepDivergentInventoriesOfOneImageApart(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)

	documents, err := Documents([]*builderv0.ImageSBOM{
		divergent(digest, "only-in-api", &builderv0.ImageSubject{Service: "web/api"}),
		divergent(digest, "only-in-worker", &builderv0.ImageSubject{Service: "billing/worker"}),
	})
	require.NoError(t, err)
	require.Len(t, documents, 2, "divergent scans of one image are two pieces of evidence")
	require.NotEqual(t, documents[0].Name, documents[1].Name, "divergent documents must not overwrite each other")

	for _, document := range documents {
		require.Len(t, document.Associations, 1, "a divergent scan covers only the service that produced it")
	}
	require.Contains(t, string(documents[0].Payload), "only-in-api")
	require.Contains(t, string(documents[1].Payload), "only-in-worker")
}

// Naming a divergent document after its content keeps a build reproducible: the
// same two scans must publish the same two names whichever arrived first.
func TestDocumentsNameDivergentInventoriesIndependentlyOfOrder(t *testing.T) {
	digest := "sha256:" + strings.Repeat("b", 64)
	api := divergent(digest, "only-in-api", &builderv0.ImageSubject{Service: "web/api"})
	worker := divergent(digest, "only-in-worker", &builderv0.ImageSubject{Service: "billing/worker"})

	forward, err := Documents([]*builderv0.ImageSBOM{api, worker})
	require.NoError(t, err)
	reversed, err := Documents([]*builderv0.ImageSBOM{worker, api})
	require.NoError(t, err)

	require.ElementsMatch(t,
		[]string{forward[0].Name, forward[1].Name},
		[]string{reversed[0].Name, reversed[1].Name},
	)
}

// An identical scan reported for two services is still one scan, and must still
// collapse onto one document naming both.
func TestDocumentsStillMergeIdenticalScansAcrossServices(t *testing.T) {
	digest := "sha256:" + strings.Repeat("c", 64)

	documents, err := Documents([]*builderv0.ImageSBOM{
		divergent(digest, "shared", &builderv0.ImageSubject{Service: "web/api"}),
		divergent(digest, "shared", &builderv0.ImageSubject{Service: "billing/worker"}),
	})
	require.NoError(t, err)
	require.Len(t, documents, 1)
	require.Len(t, documents[0].Associations, 2)
	require.Equal(t, Filename(scanned(digest, "linux/amd64")), documents[0].Name, "an undisputed scan keeps its scan-identity name")
}
