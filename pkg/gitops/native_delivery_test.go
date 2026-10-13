package gitops

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/codefly-dev/core/solutionhost"
	"github.com/stretchr/testify/require"
)

// envelopeHost stands in for the host's administrator API: fixture sign-in,
// the envelope in force, and conditional writes.
type envelopeHost struct {
	*httptest.Server
	mu       sync.Mutex
	revision uint64
	document string
	writes   []map[string]any
	signIns  int
}

func newEnvelopeHost(t *testing.T) *envelopeHost {
	t.Helper()
	host := &envelopeHost{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/auth/authenticate", func(w http.ResponseWriter, r *http.Request) {
		host.mu.Lock()
		host.signIns++
		host.mu.Unlock()
		var body struct {
			Provider string            `json:"provider"`
			Fixture  map[string]string `json:"fixture"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Provider != "email" || body.Fixture["token"] != "dev-admin" {
			http.Error(w, "unknown fixture", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"accessToken": "session"})
	})
	authorised := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "Bearer session" {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return false
		}
		return true
	}
	mux.HandleFunc("GET /v1/platform/module-envelope", func(w http.ResponseWriter, r *http.Request) {
		if !authorised(w, r) {
			return
		}
		host.mu.Lock()
		defer host.mu.Unlock()
		if host.revision == 0 {
			http.Error(w, "none", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"envelope": map[string]any{"revision": host.revision, "document": host.document}})
	})
	mux.HandleFunc("PUT /v1/platform/module-envelope", func(w http.ResponseWriter, r *http.Request) {
		if !authorised(w, r) {
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		host.mu.Lock()
		defer host.mu.Unlock()
		if uint64(body["expectedRevision"].(float64)) != host.revision {
			http.Error(w, "conflict", http.StatusConflict)
			return
		}
		host.writes = append(host.writes, body)
		host.revision++
		host.document = body["document"].(string)
		_ = json.NewEncoder(w).Encode(map[string]any{})
	})
	host.Server = httptest.NewServer(mux)
	t.Cleanup(host.Close)
	return host
}

func (h *envelopeHost) writer() *EnvelopeWriter {
	return &EnvelopeWriter{GatewayURL: h.URL, FixtureProvider: "email", FixtureToken: "dev-admin"}
}

func envelopeOf(t *testing.T, revision uint64, builds []solutionhost.ImageDigest, prefixes ...string) []byte {
	t.Helper()
	var principals []EnvelopePrincipal
	for _, prefix := range prefixes {
		principals = append(principals, EnvelopePrincipal{Prefix: prefix, SPIFFEID: "spiffe://cluster.local/ns/cell-" + prefix + "/sa/backend", Container: "backend"})
	}
	document, err := BuildEnvelope(revision, builds, principals)
	require.NoError(t, err)
	return document
}

// A plan names the revision the documents about to be delivered must carry.
// When the envelope in force already says what the run would write, that
// revision is the answer and nothing is written.
func TestAnEnvelopePlanWritesOnlyWhenTheEnvelopeChanges(t *testing.T) {
	host := newEnvelopeHost(t)
	writer := host.writer()
	build := func(prefixes ...string) func(uint64, []byte) ([]byte, error) {
		return func(revision uint64, _ []byte) ([]byte, error) {
			return envelopeOf(t, revision, []solutionhost.ImageDigest{"sha256:aa"}, prefixes...), nil
		}
	}

	revision, commit, err := writer.Plan(context.Background(), build("wiki"))
	require.NoError(t, err)
	require.Equal(t, uint64(1), revision, "a host with no envelope is written its first")
	require.Empty(t, host.writes, "a plan writes nothing")
	require.NoError(t, commit(context.Background()))
	require.Len(t, host.writes, 1)
	require.Equal(t, float64(0), host.writes[0]["expectedRevision"])

	revision, commit, err = writer.Plan(context.Background(), build("wiki"))
	require.NoError(t, err)
	require.Equal(t, uint64(1), revision, "the same content is the revision in force")
	require.NoError(t, commit(context.Background()))
	require.Len(t, host.writes, 1, "an unchanged envelope is not rewritten")

	revision, err = writer.Ensure(context.Background(), build("wiki", "proposals"))
	require.NoError(t, err)
	require.Equal(t, uint64(2), revision)
	require.Len(t, host.writes, 2)
	require.Equal(t, 1, host.signIns, "one session serves the run")
}

// A commit expects the revision the plan read. Another writer in between is a
// conflict the run reports, never a write over somebody else's revision.
func TestAnEnvelopeCommitRefusesARevisionThatMoved(t *testing.T) {
	host := newEnvelopeHost(t)
	writer := host.writer()
	_, commit, err := writer.Plan(context.Background(), func(revision uint64, _ []byte) ([]byte, error) {
		return envelopeOf(t, revision, nil, "wiki"), nil
	})
	require.NoError(t, err)
	_, err = host.writer().Ensure(context.Background(), func(revision uint64, _ []byte) ([]byte, error) {
		return envelopeOf(t, revision, nil, "proposals"), nil
	})
	require.NoError(t, err)
	require.ErrorContains(t, commit(context.Background()), "409")
}

// A run writes the envelope for what it has started so far. Whatever else the
// host already grants stays exactly as the host holds it: leaving a principal
// out would withdraw it, and a development loop never narrows.
func TestARunKeepsEveryPrincipalTheHostAlreadyGrants(t *testing.T) {
	held := []byte(`{"revision":3,"approved_builds":["sha256:aa"],"principals":{"wiki":{"grant":{"queues":["q1"]},"bindings":[{"id":"u1","revision":9007199254740993}]},"proposals":{"grant":{"tenant":"t1"}}}}`)
	next := envelopeOf(t, 4, []solutionhost.ImageDigest{"sha256:aa", "sha256:bb"}, "wiki", "lastlogin")

	merged, err := KeepGrantedPrincipals(next, held)
	require.NoError(t, err)
	var document struct {
		Revision   uint64                     `json:"revision"`
		Builds     []string                   `json:"approved_builds"`
		Principals map[string]json.RawMessage `json:"principals"`
	}
	require.NoError(t, json.Unmarshal(merged, &document))
	require.Equal(t, uint64(4), document.Revision)
	require.Equal(t, []string{"sha256:aa", "sha256:bb"}, document.Builds)
	require.ElementsMatch(t, []string{"wiki", "lastlogin", "proposals"}, keysOf(document.Principals))
	require.JSONEq(t, `{"grant":{"tenant":"t1"}}`, string(document.Principals["proposals"]), "a principal the run did not start is carried as the host holds it")

	var nextPrincipals struct {
		Principals map[string]json.RawMessage `json:"principals"`
	}
	require.NoError(t, json.Unmarshal(next, &nextPrincipals))
	require.JSONEq(t, string(nextPrincipals.Principals["wiki"]), string(document.Principals["wiki"]), "a principal the run started is the run's")

	unchanged, err := KeepGrantedPrincipals(next, nil)
	require.NoError(t, err)
	require.JSONEq(t, string(next), string(unchanged))

	_, err = KeepGrantedPrincipals(next, []byte(`not json`))
	require.Error(t, err, "an envelope in force that cannot be read is not silently replaced")
}

func keysOf(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	return keys
}

func TestApprovedBuildsReadsWhatAnEnvelopeApproves(t *testing.T) {
	require.Equal(t, []solutionhost.ImageDigest{"sha256:aa", "sha256:bb"},
		ApprovedBuilds([]byte(`{"approved_builds":["sha256:aa","sha256:bb"]}`)))
	require.Empty(t, ApprovedBuilds(nil))
	require.Empty(t, ApprovedBuilds([]byte(`nope`)))
}
