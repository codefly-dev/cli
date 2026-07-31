// Package conformance provides the normative Codefly provider-protocol
// conformance fixtures. It is not a fake vendor: it is a Codefly-owned
// reference server that exercises the provider host boundary deterministically,
// so the neutral provider (well-behaved) and the hostile providers (attacking
// the trust boundary) can be judged against one fixed target.
//
// The centerpiece is Fixture, a loopback HTTP server with deterministic request
// ids and timestamps, in-memory resources with ownership metadata, one-time and
// read-path secrets, idempotent-POST replay, and injectable faults. It exposes
// safe inspection endpoints (and equivalent Go accessors) that report the exact
// requests received without ever echoing a request or response value, so a test
// can assert both that an effect happened and that no request leaked past a
// denial.
//
// Every secret the fixture emits is a planted poison value (PoisonSecret,
// PoisonDSN). Nothing the fixture reports through its inspection surface, and no
// committed golden artifact, may contain a poison value: their sole purpose is
// to be caught if the host boundary ever forwards one.
package conformance

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"time"
)

// Planted poison values. They appear only in raw upstream response bodies the
// fixture serves; the host boundary must capture or suppress them so they never
// reach a provider, an inspection response, or a golden artifact.
const (
	PoisonSecret = "codefly-poison-secret-DO-NOT-LEAK"                                          //nolint:gosec // G101: deliberately fake planted poison, never a real credential
	PoisonDSN    = "codefly-poison://user:codefly-poison-secret-DO-NOT-LEAK@fixture.invalid/db" //nolint:gosec // G101: deliberately fake planted poison, never a real credential
)

// epoch is the fixed clock origin. The fixture derives every timestamp from a
// per-request sequence number so ids and timestamps are byte-stable across runs
// and platforms.
var epoch = time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)

// RecordedRequest is the safe projection of one received request: identifiers,
// method, path, and the field names present in the body — never a header value
// or a body value, so inspection cannot become a side channel for a secret.
type RecordedRequest struct {
	Sequence   int      `json:"sequence"`
	RequestID  string   `json:"request_id"`
	Method     string   `json:"method"`
	Path       string   `json:"path"`
	Query      string   `json:"query,omitempty"`
	HeaderKeys []string `json:"header_keys"`
	BodyKeys   []string `json:"body_keys,omitempty"`
}

// faults holds one-shot fault injections consumed by the next received request.
type faults struct {
	rateLimited bool
	malformed   bool
	redirect    bool
}

// Fixture is the normative reference server. The zero value is not usable; call
// NewFixture. It is safe for concurrent use.
type Fixture struct {
	server *httptest.Server

	mu       sync.Mutex
	seq      int
	accounts map[string]string // resource id -> owning principal
	nextID   int
	idem     map[string]string
	requests []RecordedRequest
	faults   faults
}

// NewFixture starts a loopback fixture server. Call Close when done. The server
// listens on a real loopback socket so the host's transport path (tracing,
// SSRF guard, credential injection) is exercised end to end.
func NewFixture() *Fixture {
	f := &Fixture{
		accounts: make(map[string]string),
		idem:     make(map[string]string),
		nextID:   1,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/account", f.handleIdentity)
	mux.HandleFunc("/v1/accounts", f.handleCollection)
	mux.HandleFunc("/v1/accounts/", f.handleResource)
	mux.HandleFunc("/_inspect/requests", f.handleInspectRequests)
	mux.HandleFunc("/_inspect/resources", f.handleInspectResources)
	mux.HandleFunc("/_inspect/reset", f.handleReset)
	f.server = httptest.NewServer(mux)
	return f
}

// Close shuts the server down.
func (f *Fixture) Close() { f.server.Close() }

// Addr is the host:port the server listens on, suitable for a host-owned dialer.
func (f *Fixture) Addr() string { return f.server.Listener.Addr().String() }

// URL is the base URL of the server.
func (f *Fixture) URL() string { return f.server.URL }

// Seed inserts an owned account so read paths can be exercised without first
// creating one. It returns the account id.
func (f *Fixture) Seed(owner string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.insertLocked(owner)
}

// InjectRateLimited makes the next received request answer 429 with retry-after
// and rate metadata, without recording an effect. The fault is consumed by the
// next request to any resource endpoint, regardless of method.
func (f *Fixture) InjectRateLimited() { f.setFault(func(x *faults) { x.rateLimited = true }) }

// InjectMalformed makes the next received request answer with a duplicate-key,
// malformed JSON body so response decoding is exercised against hostile input.
func (f *Fixture) InjectMalformed() { f.setFault(func(x *faults) { x.malformed = true }) }

// InjectRedirect makes the next received request answer 302 to the collection,
// so the host's no-follow-redirect stance is exercised.
func (f *Fixture) InjectRedirect() { f.setFault(func(x *faults) { x.redirect = true }) }

// Requests returns the safe projection of every request received so far.
func (f *Fixture) Requests() []RecordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]RecordedRequest, len(f.requests))
	copy(out, f.requests)
	return out
}

// RequestCount reports how many requests reached the server. A denial upstream
// of the host boundary must leave this unchanged.
func (f *Fixture) RequestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// ResourceCount reports how many resources currently exist, so idempotent replay
// (two requests, one effect) is observable.
func (f *Fixture) ResourceCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.accounts)
}

// Reset clears resources, idempotency records, requests, and pending faults.
func (f *Fixture) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accounts = make(map[string]string)
	f.idem = make(map[string]string)
	f.requests = nil
	f.faults = faults{}
	f.seq = 0
	f.nextID = 1
}

func (f *Fixture) setFault(mutate func(*faults)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	mutate(&f.faults)
}

func (f *Fixture) insertLocked(owner string) string {
	id := fmt.Sprintf("acct_%04d", f.nextID)
	f.nextID++
	f.accounts[id] = owner
	return id
}

// record stamps a deterministic request id and stores the safe projection. It
// returns the id and timestamp for the response.
func (f *Fixture) record(r *http.Request) (string, time.Time) {
	f.seq++
	id := fmt.Sprintf("req-%04d", f.seq)
	stamp := epoch.Add(time.Duration(f.seq) * time.Second)
	rec := RecordedRequest{
		Sequence:   f.seq,
		RequestID:  id,
		Method:     r.Method,
		Path:       r.URL.Path,
		Query:      r.URL.RawQuery,
		HeaderKeys: safeHeaderKeys(r),
		BodyKeys:   bodyKeys(r),
	}
	f.requests = append(f.requests, rec)
	return id, stamp
}

func (f *Fixture) writeAccount(w http.ResponseWriter, id, requestID string, stamp time.Time) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-Id", requestID)
	w.Header().Set("X-Timestamp", stamp.Format(time.RFC3339))
	w.Header().Set("X-Api-Version", "2024-01-01")
	// $.id and $.metadata.public are safe and forwarded; $.secret is captured;
	// $.metadata.internal is suppressed. The remaining fields are undeclared and
	// must be dropped by the response policy.
	body := fmt.Sprintf(`{"object":"account","id":%q,"livemode":false,`+
		`"secret":%q,"dsn":%q,`+
		`"metadata":{"public":"safe-adjacent","internal":"private"},`+
		`"api_version":"2024-01-01","owned_by":"codefly"}`, id, PoisonSecret, PoisonDSN)
	_, _ = w.Write([]byte(body)) //nolint:gosec // G705: loopback JSON conformance fixture, never rendered in a browser
}

func (f *Fixture) handleIdentity(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	requestID, stamp := f.record(r)
	if f.consumeFaults(w, requestID) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-Id", requestID)
	w.Header().Set("X-Timestamp", stamp.Format(time.RFC3339))
	_, _ = w.Write([]byte(`{"object":"account","livemode":false,"mode":"managed"}`))
}

func (f *Fixture) handleCollection(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	requestID, stamp := f.record(r)
	if f.consumeFaults(w, requestID) {
		return
	}
	switch r.Method {
	case http.MethodPost:
		f.createLocked(w, r, requestID, stamp)
	case http.MethodGet:
		f.listLocked(w, r, requestID, stamp)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (f *Fixture) handleResource(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	requestID, stamp := f.record(r)
	if f.consumeFaults(w, requestID) {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/accounts/")
	switch r.Method {
	case http.MethodGet:
		if _, ok := f.accounts[id]; !ok {
			f.writeError(w, http.StatusNotFound, requestID)
			return
		}
		f.writeAccount(w, id, requestID, stamp)
	case http.MethodDelete:
		if _, ok := f.accounts[id]; !ok {
			f.writeError(w, http.StatusNotFound, requestID)
			return
		}
		delete(f.accounts, id)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", requestID)
		_, _ = fmt.Fprintf(w, `{"deleted":true,"id":%q}`, id) //nolint:gosec // G705: loopback JSON conformance fixture, never rendered in a browser
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (f *Fixture) createLocked(w http.ResponseWriter, r *http.Request, requestID string, stamp time.Time) {
	// Idempotent POST: the same key replays the first response and records no new
	// effect. A missing key is non-idempotent and always creates.
	if key := r.Header.Get("Idempotency-Key"); key != "" {
		if id, ok := f.idem[key]; ok {
			f.writeAccount(w, id, requestID, stamp)
			return
		}
		id := f.insertLocked("codefly")
		f.idem[key] = id
		f.writeAccount(w, id, requestID, stamp)
		return
	}
	id := f.insertLocked("codefly")
	f.writeAccount(w, id, requestID, stamp)
}

func (f *Fixture) listLocked(w http.ResponseWriter, r *http.Request, requestID string, stamp time.Time) {
	ids := make([]string, 0, len(f.accounts))
	for id := range f.accounts {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	// Cursor pagination: one item per page so at least two pages and a final
	// empty cursor are always exercised.
	cursor := r.URL.Query().Get("cursor")
	start := 0
	if cursor != "" {
		for i, id := range ids {
			if id == cursor {
				start = i
				break
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-Id", requestID)
	w.Header().Set("X-Timestamp", stamp.Format(time.RFC3339))
	if start >= len(ids) {
		_, _ = w.Write([]byte(`{"object":"list","data":[],"next_cursor":""}`))
		return
	}
	id := ids[start]
	next := ""
	if start+1 < len(ids) {
		next = ids[start+1]
	}
	// Each element carries a poison secret adjacent to a safe id, so read-array
	// secret filtering is exercised.
	_, _ = fmt.Fprintf(w, `{"object":"list","data":[{"id":%q,"secret":%q,"public":"safe-adjacent"}],"next_cursor":%q}`,
		id, PoisonSecret, next)
}

func (f *Fixture) consumeFaults(w http.ResponseWriter, requestID string) bool {
	switch {
	case f.faults.rateLimited:
		f.faults.rateLimited = false
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", requestID)
		w.Header().Set("Retry-After", "2")
		w.Header().Set("X-Rate-Limit-Remaining", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"rate_limit","message":"slow down"}}`))
		return true
	case f.faults.malformed:
		f.faults.malformed = false
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", requestID)
		// Duplicate keys and a trailing comma: ambiguous, hostile JSON.
		_, _ = w.Write([]byte(`{"id":"acct_x","id":"acct_y",}`))
		return true
	case f.faults.redirect:
		f.faults.redirect = false
		w.Header().Set("Location", "/v1/accounts")
		w.Header().Set("X-Request-Id", requestID)
		w.WriteHeader(http.StatusFound)
		return true
	}
	return false
}

func (f *Fixture) writeError(w http.ResponseWriter, status int, requestID string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-Id", requestID)
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"error":{"type":"not_found"}}`)
}

func (f *Fixture) handleInspectRequests(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	writeJSON(w, map[string]any{"requests": f.requests, "count": len(f.requests)})
}

func (f *Fixture) handleInspectResources(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]string, 0, len(f.accounts))
	for id, owner := range f.accounts {
		ids = append(ids, id+":"+owner)
	}
	sort.Strings(ids)
	writeJSON(w, map[string]any{"resources": ids, "count": len(ids)})
}

func (f *Fixture) handleReset(w http.ResponseWriter, _ *http.Request) {
	f.Reset()
	writeJSON(w, map[string]any{"reset": true})
}

func writeJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}

// safeHeaderKeys returns the sorted set of header names, never their values.
func safeHeaderKeys(r *http.Request) []string {
	keys := make([]string, 0, len(r.Header))
	for k := range r.Header {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// bodyKeys returns the sorted top-level field names of a JSON body, never their
// values. A non-JSON or unreadable body yields no keys.
func bodyKeys(r *http.Request) []string {
	if r.Body == nil {
		return nil
	}
	var decoded map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&decoded); err != nil {
		return nil
	}
	keys := make([]string, 0, len(decoded))
	for k := range decoded {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
