package orchestration

import (
	"strings"
	"testing"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// digestManager is a stand-in IManager that only reports a unique, a builder
// image digest and its image evidence — the methods OriginImageDigest and
// OriginImageEvidence touch.
type digestManager struct {
	IManager
	unique   string
	digest   string
	evidence []*builderv0.ImageSBOM
}

func (m *digestManager) Unique() string             { return m.unique }
func (m *digestManager) BuilderImageDigest() string { return m.digest }

func (m *digestManager) BuilderImageEvidence() []*builderv0.ImageSBOM { return m.evidence }

func TestOriginImageDigestReturnsOriginBuildersDigest(t *testing.T) {
	service := &resources.Service{Name: "frontend"}
	service.WithModule("web")
	origin := resources.WithUnique(service).Unique()
	digest := "sha256:" + strings.Repeat("a", 64)

	// A build with dependencies pushes several images; the returned digest must
	// be the origin service's, not a dependency's.
	flow := &Flow{
		originService: service,
		hub: &Hub{managers: []IManager{
			&digestManager{unique: "web/api", digest: "sha256:" + strings.Repeat("b", 64)},
			&digestManager{unique: origin, digest: digest},
		}},
	}

	require.Equal(t, digest, flow.OriginImageDigest())
}

func TestOriginImageDigestEmptyWhenNothingPushed(t *testing.T) {
	service := &resources.Service{Name: "frontend"}
	service.WithModule("web")
	flow := &Flow{
		originService: service,
		hub:           &Hub{managers: []IManager{&digestManager{unique: resources.WithUnique(service).Unique()}}},
	}
	require.Equal(t, "", flow.OriginImageDigest())
}

func TestOriginImageDigestNilHubIsEmpty(t *testing.T) {
	require.Equal(t, "", (&Flow{}).OriginImageDigest())
}

func TestOriginImageEvidenceReturnsOriginBuildersEvidence(t *testing.T) {
	service := &resources.Service{Name: "frontend"}
	service.WithModule("web")
	origin := resources.WithUnique(service).Unique()
	evidence := []*builderv0.ImageSBOM{{Digest: "sha256:" + strings.Repeat("a", 64), Platform: "linux/amd64"}}

	// Dependencies are built and scanned too; the caller's subject is the origin.
	flow := &Flow{
		originService: service,
		hub: &Hub{managers: []IManager{
			&digestManager{unique: "web/api", evidence: []*builderv0.ImageSBOM{{Digest: "sha256:" + strings.Repeat("b", 64)}}},
			&digestManager{unique: origin, evidence: evidence},
		}},
	}

	require.Equal(t, evidence, flow.OriginImageEvidence())
}

func TestOriginImageEvidenceNilWhenNothingCollected(t *testing.T) {
	service := &resources.Service{Name: "frontend"}
	service.WithModule("web")
	flow := &Flow{
		originService: service,
		hub:           &Hub{managers: []IManager{&digestManager{unique: resources.WithUnique(service).Unique()}}},
	}
	require.Nil(t, flow.OriginImageEvidence())
}

func TestOriginImageEvidenceNilHubIsEmpty(t *testing.T) {
	require.Nil(t, (&Flow{}).OriginImageEvidence())
}

// A build that is not stand-alone pushes its dependencies' images too, so their
// evidence covers shipped images and a publisher must be able to reach it.
func TestImageEvidenceCoversEveryServiceThatCollected(t *testing.T) {
	service := &resources.Service{Name: "frontend"}
	service.WithModule("web")
	origin := resources.WithUnique(service).Unique()
	originEvidence := []*builderv0.ImageSBOM{{Digest: "sha256:" + strings.Repeat("a", 64), Platform: "linux/amd64"}}
	dependencyEvidence := []*builderv0.ImageSBOM{{Digest: "sha256:" + strings.Repeat("b", 64), Platform: "linux/amd64"}}

	flow := &Flow{
		originService: service,
		hub: &Hub{managers: []IManager{
			&digestManager{unique: "web/api", evidence: dependencyEvidence},
			&digestManager{unique: origin, evidence: originEvidence},
		}},
	}

	require.Equal(t, map[string][]*builderv0.ImageSBOM{
		"web/api": dependencyEvidence,
		origin:    originEvidence,
	}, flow.ImageEvidence())
}

func TestImageEvidenceOmitsServicesThatCollectedNothing(t *testing.T) {
	service := &resources.Service{Name: "frontend"}
	service.WithModule("web")
	origin := resources.WithUnique(service).Unique()
	evidence := []*builderv0.ImageSBOM{{Digest: "sha256:" + strings.Repeat("a", 64)}}

	flow := &Flow{
		originService: service,
		hub: &Hub{managers: []IManager{
			&digestManager{unique: "web/api"},
			&digestManager{unique: origin, evidence: evidence},
		}},
	}

	require.Equal(t, map[string][]*builderv0.ImageSBOM{origin: evidence}, flow.ImageEvidence())
}

func TestImageEvidenceNilHubIsEmpty(t *testing.T) {
	require.Empty(t, (&Flow{}).ImageEvidence())
}
