package gitops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/codefly-dev/core/solutionhost"

	compositionv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/composition/v1"
)

// --- Applying prepared paired documents ---
//
// A rendered delivery carries BOTH signed documents: core's module manifest in
// its DSSE carrier, and the authority declaration — method policies, scope
// ceilings, roles, owner-awareness, the installation selector and its
// person-context edges — signed against that carrier's exact bytes. Only the
// composition interface takes the pair (CompositionDelivery); the host's
// legacy HTTP manifest route takes the Core half alone and answers for it, so
// a module delivered that way reaches the host with no roles at all.
//
// The pair is settled and signed where release custody is — at publish — and
// SENT from inside the cell, after the module's workloads exist. Those are two
// different moments and two different places, so the request travels as a
// prepared document and one field of it is deliberately left empty: the
// CREDENTIAL. A delivery credential is a projected ServiceAccount token with a
// lifetime of minutes; a token written into a rendered manifest would be a
// published secret and would be expired by the time the manifest synced. So
// the prepared document carries `credential: ""` and the sender fills it from
// the projection AT EXECUTION, once per request, so a token rotated between
// rounds is the one the next round sends.
//
// The token is read into the request and nothing else: it is never printed,
// never in an argument, never in an error. A refusal names the document, the
// code and the host's reason.

// PreparedDeliveryDocument is the file name suffix of a prepared paired
// request. The sender reads every one of them in its directory, in name order,
// so what is sent is what the render wrote and the order is the render's.
const PreparedDeliveryDocument = ".json"

// PairedApply sends prepared paired requests to one host's composition
// interface. Its retry shape is the delivery Job's: every pending document
// once per round, then a pause that doubles up to MaxPause, rounds until each
// is answered or Budget is spent — so a document the host keeps failing never
// keeps a later one, a withdrawal among them, from its first attempt.
type PairedApply struct {
	// Client is the generated composition client, over any transport that
	// carries it (the host's connect endpoint serves gRPC on the same
	// listener).
	Client compositionv1.CompositionServiceClient
	// Credential reads the delivery credential. Called once per request, not
	// once per run, so a rotated projection is re-read.
	Credential func() (string, error)
	// Budget bounds the whole run, Request one call, MaxPause the pause
	// between rounds.
	Budget, Request, MaxPause time.Duration
	// Now and Sleep are the clock, for a test that does not wait.
	Now   func() time.Time
	Sleep func(context.Context, time.Duration) error
	// Report is called once per answered document: accepted, replayed or
	// refused, with the host's reason. nil prints nothing.
	Report func(string)
}

// PairedReceipt is one document's outcome.
type PairedReceipt struct {
	Document   string
	Binding    string
	Generation uint64
	Accepted   bool
	// Err is the host's refusal, or the reason the run gave up on it.
	Err error
}

// defaults for a sender a caller configured only partly.
const (
	pairedDefaultBudget   = 1500 * time.Second
	pairedDefaultRequest  = 30 * time.Second
	pairedDefaultMaxPause = 60 * time.Second
)

// Apply sends every prepared document in directory and answers one receipt per
// document, in the order the documents were read. The error is non-nil when any
// document was refused or left unanswered: a delivery that did not land must
// fail the caller that ran it, since nothing else will notice.
func (a *PairedApply) Apply(ctx context.Context, directory string) ([]PairedReceipt, error) {
	documents, err := readPreparedDocuments(directory)
	if err != nil {
		return nil, err
	}
	if a.Client == nil {
		return nil, errors.New("a paired delivery needs the host's composition client")
	}
	if a.Credential == nil {
		return nil, errors.New("a paired delivery needs the delivery credential, read at execution")
	}
	now, sleep := a.Now, a.Sleep
	if now == nil {
		now = time.Now
	}
	if sleep == nil {
		sleep = func(ctx context.Context, d time.Duration) error {
			timer := time.NewTimer(d)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		}
	}
	budget, request, maxPause := a.Budget, a.Request, a.MaxPause
	if budget <= 0 {
		budget = pairedDefaultBudget
	}
	if request <= 0 {
		request = pairedDefaultRequest
	}
	if maxPause <= 0 {
		maxPause = pairedDefaultMaxPause
	}
	started := now()
	left := func() time.Duration { return budget - now().Sub(started) }
	receipts := make([]PairedReceipt, len(documents))
	for i := range documents {
		receipts[i] = PairedReceipt{Document: documents[i].name, Binding: documents[i].binding, Generation: documents[i].generation}
	}
	pending := make([]int, len(documents))
	for i := range pending {
		pending[i] = i
	}
	pause := time.Second
	for round := 1; len(pending) > 0; round++ {
		var again []int
		for _, i := range pending {
			remaining := left()
			if remaining <= 0 {
				again = append(again, i)
				continue
			}
			timeout := request
			if timeout > remaining {
				timeout = remaining
			}
			receipt := a.send(ctx, documents[i], timeout)
			switch {
			case receipt.Err == nil:
				a.say(fmt.Sprintf("delivered %s: binding %s at generation %d", receipt.Document, receipt.Binding, receipt.Generation))
			case retryablePairedRefusal(receipt.Err):
				a.say(fmt.Sprintf("the composition interface answered %v for %s in round %d; it is tried again next round", receipt.Err, receipt.Document, round))
				again = append(again, i)
				continue
			default:
				a.say(fmt.Sprintf("refused %s: %v", receipt.Document, receipt.Err))
			}
			receipts[i] = receipt
			if err := ctx.Err(); err != nil {
				return receipts, err
			}
		}
		pending = again
		if len(pending) == 0 {
			break
		}
		remaining := left()
		if remaining <= 0 {
			for _, i := range pending {
				receipts[i].Err = fmt.Errorf("giving up on %s after %d rounds within %s", documents[i].name, round, budget)
				a.say(receipts[i].Err.Error())
			}
			break
		}
		nap := pause
		if nap > remaining {
			nap = remaining
		}
		if err := sleep(ctx, nap); err != nil {
			return receipts, err
		}
		if pause *= 2; pause > maxPause {
			pause = maxPause
		}
	}
	var refused []string
	for _, receipt := range receipts {
		if receipt.Err != nil {
			refused = append(refused, receipt.Document)
		}
	}
	if len(refused) > 0 {
		return receipts, fmt.Errorf("the host did not accept %s", strings.Join(refused, ", "))
	}
	return receipts, nil
}

// send fills the credential and applies one document. The credential is read
// here, per request, so a projection rotated between rounds is re-read, and it
// is set on a COPY of the prepared request so no token is retained after the
// call.
func (a *PairedApply) send(ctx context.Context, document preparedDocument, timeout time.Duration) PairedReceipt {
	receipt := PairedReceipt{Document: document.name, Binding: document.binding, Generation: document.generation}
	credential, err := a.Credential()
	if err != nil {
		receipt.Err = fmt.Errorf("read the delivery credential: %w", err)
		return receipt
	}
	if credential == "" {
		receipt.Err = errors.New("the delivery credential is empty; the projected token is absent or not yet written")
		return receipt
	}
	request := &compositionv1.ApplyManifestRequest{
		Carrier: document.request.GetCarrier(), Authority: document.request.GetAuthority(),
		// RAW: the host adds the bearer scheme itself before it authorizes
		// the carrier, so a scheme added here is a second one.
		Credential: credential,
	}
	call, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := a.Client.ApplyManifest(call, request)
	if err != nil {
		receipt.Err = refusal(document.binding, document.manifest, err)
		return receipt
	}
	if out.GetGeneration() != document.generation {
		receipt.Err = fmt.Errorf("the host acknowledged generation %d for binding %s, sent %d", out.GetGeneration(), document.binding, document.generation)
		return receipt
	}
	receipt.Accepted = true
	return receipt
}

func (a *PairedApply) say(line string) {
	if a.Report != nil {
		a.Report(line)
	}
}

// retryablePairedRefusal reports whether asking again can answer differently.
// It reads the ONE classification the run already uses (HostStatusError
// .Transient): unavailable, out of time, out of capacity. Every other code is
// the host's answer — an invalid document, a refused carrier, a stale or
// rewritten generation, a withdrawal it keeps terminal — and asking again gets
// the same one, so it refuses the document rather than spending the budget on
// it.
func retryablePairedRefusal(err error) bool {
	var refusal *HostStatusError
	if errors.As(err, &refusal) {
		return refusal.Transient()
	}
	return false
}

// preparedDocument is one prepared request as read from disk, with the
// manifest its carrier holds: the binding and generation are read from the
// SIGNED document, never from the file's name or from an unsigned field, so
// what the sender reports is what the host verifies.
type preparedDocument struct {
	name       string
	request    *compositionv1.ApplyManifestRequest
	manifest   *solutionhost.ModuleManifest
	binding    string
	generation uint64
}

// readPreparedDocuments reads every prepared request in directory, in name
// order. A directory with no document is an error: a delivery that carries
// nothing has gone wrong upstream, and silently succeeding would report a
// delivery that never happened.
func readPreparedDocuments(directory string) ([]preparedDocument, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("read the prepared delivery documents: %w", err)
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), PreparedDeliveryDocument) {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil, fmt.Errorf("no prepared delivery document under %s", directory)
	}
	documents := make([]preparedDocument, 0, len(names))
	for _, name := range names {
		data, readErr := os.ReadFile(filepath.Join(directory, name))
		if readErr != nil {
			return nil, readErr
		}
		document, parseErr := parsePreparedDocument(name, data)
		if parseErr != nil {
			return nil, parseErr
		}
		documents = append(documents, document)
	}
	return documents, nil
}

// parsePreparedDocument decodes one prepared request and refuses what the host
// would refuse later and further from the cause: a request carrying no
// carrier, a carrier whose document is not a module manifest, or a credential
// already written into the rendered document — which would be a published
// secret, so it is refused here rather than sent.
func parsePreparedDocument(name string, data []byte) (preparedDocument, error) {
	var request compositionv1.ApplyManifestRequest
	if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(data, &request); err != nil {
		return preparedDocument{}, fmt.Errorf("%s is not a prepared delivery request: %w", name, err)
	}
	if request.GetCredential() != "" {
		return preparedDocument{}, fmt.Errorf("%s carries a credential; a prepared delivery document never holds one, and the sender reads it from the projection at execution", name)
	}
	if request.GetCarrier() == nil {
		return preparedDocument{}, fmt.Errorf("%s carries no signed module manifest", name)
	}
	manifest, err := solutionhost.ManifestFromVerified(request.GetCarrier().GetDocument())
	if err != nil {
		return preparedDocument{}, fmt.Errorf("%s: %w", name, err)
	}
	return preparedDocument{
		name: name, request: &request, manifest: manifest,
		binding: manifest.Binding, generation: manifest.Generation,
	}, nil
}
