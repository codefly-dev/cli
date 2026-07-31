package conformance

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func get(t *testing.T, url string) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.Get(url) //nolint:noctx // loopback fixture in a test
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return resp, body
}

func postJSON(t *testing.T, url string, headers map[string]string, payload string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return resp, body
}

func TestFixtureDeterministicRequestIdentity(t *testing.T) {
	f := NewFixture()
	defer f.Close()

	resp1, _ := get(t, f.URL()+"/v1/account")
	resp2, _ := get(t, f.URL()+"/v1/account")

	require.Equal(t, "req-0001", resp1.Header.Get("X-Request-Id"))
	require.Equal(t, "req-0002", resp2.Header.Get("X-Request-Id"))
	require.Equal(t, "2020-01-01T00:00:01Z", resp1.Header.Get("X-Timestamp"))
	require.Equal(t, "2020-01-01T00:00:02Z", resp2.Header.Get("X-Timestamp"))
}

func TestFixtureIdempotentPOSTReplaysOneEffect(t *testing.T) {
	f := NewFixture()
	defer f.Close()

	_, first := postJSON(t, f.URL()+"/v1/accounts", map[string]string{"Idempotency-Key": "idem-1"}, `{"name":"a"}`)
	_, second := postJSON(t, f.URL()+"/v1/accounts", map[string]string{"Idempotency-Key": "idem-1"}, `{"name":"a"}`)

	require.Equal(t, idOf(t, first), idOf(t, second), "same key must replay the same resource id")
	require.Equal(t, 1, f.ResourceCount(), "idempotent replay must not create a second resource")
	require.Equal(t, 2, f.RequestCount(), "both requests are recorded")
}

func TestFixtureNonIdempotentPOSTCreatesEachTime(t *testing.T) {
	f := NewFixture()
	defer f.Close()

	_, first := postJSON(t, f.URL()+"/v1/accounts", nil, `{"name":"a"}`)
	_, second := postJSON(t, f.URL()+"/v1/accounts", nil, `{"name":"a"}`)

	require.NotEqual(t, idOf(t, first), idOf(t, second))
	require.Equal(t, 2, f.ResourceCount())
}

func TestFixtureRateLimitFault(t *testing.T) {
	f := NewFixture()
	defer f.Close()
	f.InjectRateLimited()

	resp, _ := postJSON(t, f.URL()+"/v1/accounts", nil, `{"name":"a"}`)

	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	require.Equal(t, "2", resp.Header.Get("Retry-After"))
	require.Equal(t, "0", resp.Header.Get("X-Rate-Limit-Remaining"))
	require.Equal(t, 0, f.ResourceCount(), "a rate-limited request records no effect")
}

func TestFixtureMalformedFault(t *testing.T) {
	f := NewFixture()
	defer f.Close()
	f.InjectMalformed()

	resp, body := postJSON(t, f.URL()+"/v1/accounts", nil, `{"name":"a"}`)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	// Duplicate keys and a trailing comma: not decodable as a single object.
	var decoded map[string]any
	require.Error(t, json.Unmarshal(body, &decoded), "the malformed fault must not decode cleanly")
	require.Equal(t, 0, f.ResourceCount(), "a faulted request records no effect")
}

// TestFixtureFaultAppliesToReadRequest is the regression guard for the fault
// boundary: an injected fault is served by the next request to any endpoint,
// including a read, not only a mutating create.
func TestFixtureFaultAppliesToReadRequest(t *testing.T) {
	f := NewFixture()
	defer f.Close()
	f.InjectRateLimited()

	resp, _ := get(t, f.URL()+"/v1/account")

	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	require.Equal(t, "2", resp.Header.Get("Retry-After"))
}

func TestFixtureRedirectFaultIsNotFollowed(t *testing.T) {
	f := NewFixture()
	defer f.Close()
	f.InjectRedirect()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequest(http.MethodPost, f.URL()+"/v1/accounts", strings.NewReader(`{"name":"a"}`))
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	require.Equal(t, http.StatusFound, resp.StatusCode)
	require.Equal(t, "/v1/accounts", resp.Header.Get("Location"))
	require.Equal(t, 0, f.ResourceCount())
}

func TestFixtureCursorPaginationHasTwoPages(t *testing.T) {
	f := NewFixture()
	defer f.Close()
	first := f.Seed("codefly")
	second := f.Seed("codefly")

	_, page1 := get(t, f.URL()+"/v1/accounts")
	next := nextCursor(t, page1)
	require.Equal(t, second, next, "the first page points at the second resource")

	_, page2 := get(t, f.URL()+"/v1/accounts?cursor="+next)
	require.Empty(t, nextCursor(t, page2), "the second page is the last")
	// Sanity: the two pages surface the two distinct resources.
	require.Contains(t, string(page1), first)
	require.Contains(t, string(page2), second)
}

func TestFixtureInspectionNeverEchoesValues(t *testing.T) {
	f := NewFixture()
	defer f.Close()

	// A request carrying a secret-looking header and body value.
	postJSON(t, f.URL()+"/v1/accounts",
		map[string]string{"Authorization": "Bearer " + PoisonSecret},
		`{"name":"`+PoisonSecret+`"}`)

	_, requests := get(t, f.URL()+"/_inspect/requests")
	_, resources := get(t, f.URL()+"/_inspect/resources")

	assertNoPoison(t, requests)
	assertNoPoison(t, resources)

	// The projection keeps names, not values.
	require.Contains(t, string(requests), "Authorization")
	require.Contains(t, string(requests), `"name"`)
	require.NotContains(t, string(requests), "Bearer")
}

func TestFixtureResetClearsState(t *testing.T) {
	f := NewFixture()
	defer f.Close()
	f.Seed("codefly")
	postJSON(t, f.URL()+"/v1/accounts", nil, `{"name":"a"}`)
	require.Positive(t, f.RequestCount())
	require.Positive(t, f.ResourceCount())

	get(t, f.URL()+"/_inspect/reset")

	require.Equal(t, 0, f.RequestCount())
	require.Equal(t, 0, f.ResourceCount())
	// The deterministic counters restart.
	resp, _ := get(t, f.URL()+"/v1/account")
	require.Equal(t, "req-0001", resp.Header.Get("X-Request-Id"))
}

func idOf(t *testing.T, body []byte) string {
	t.Helper()
	var decoded struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(body, &decoded))
	require.NotEmpty(t, decoded.ID)
	return decoded.ID
}

func nextCursor(t *testing.T, body []byte) string {
	t.Helper()
	var decoded struct {
		NextCursor string `json:"next_cursor"`
	}
	require.NoError(t, json.Unmarshal(body, &decoded))
	return decoded.NextCursor
}

// assertNoPoison fails if any planted poison value appears in the bytes.
func assertNoPoison(t *testing.T, haystack []byte) {
	t.Helper()
	for _, needle := range []string{PoisonSecret, PoisonDSN} {
		require.False(t, bytes.Contains(haystack, []byte(needle)),
			"poison value leaked: %q", needle)
	}
}
