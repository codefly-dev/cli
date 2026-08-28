package orchestration

import (
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// digestManager is a stand-in IManager that only reports a unique and a builder
// image digest — the two methods OriginImageDigest touches.
type digestManager struct {
	IManager
	unique string
	digest string
}

func (m *digestManager) Unique() string             { return m.unique }
func (m *digestManager) BuilderImageDigest() string { return m.digest }

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
