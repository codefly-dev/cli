package go_grpc

import (
	"sync"

	"google.golang.org/protobuf/types/known/timestamppb"

	observabilityv0 "github.com/codefly-dev/core/generated/go/codefly/observability/v0"
)

// logHistory is a fixed-capacity ring buffer of the most recent log entries,
// so a dashboard opened (or reloaded) after a run started can backfill. It
// also tracks live subscribers (Subscribe) so a fresh Logs() stream can
// obtain "everything so far" and "everything from here on" as one atomic
// operation: Add and Subscribe share the same lock, so any given entry is
// delivered through exactly one of a subscriber's initial snapshot or its
// live channel, never both and never neither. This also lets any number of
// subscribers observe the full live feed independently, instead of a single
// shared channel splitting entries across whichever reader happens to win.
type logHistory struct {
	mu          sync.Mutex
	buf         []*observabilityv0.Log
	next        int
	full        bool
	subscribers map[uint64]chan *observabilityv0.Log
	nextSubID   uint64
}

func newLogHistory(capacity int) *logHistory {
	return &logHistory{
		buf:         make([]*observabilityv0.Log, capacity),
		subscribers: make(map[uint64]chan *observabilityv0.Log),
	}
}

func (h *logHistory) Add(entry *observabilityv0.Log) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.buf[h.next] = entry
	h.next++
	if h.next == len(h.buf) {
		h.next = 0
		h.full = true
	}
	for _, ch := range h.subscribers {
		// Non-blocking: a slow subscriber drops live lines rather than
		// stalling every other subscriber's Add call.
		select {
		case ch <- entry:
		default:
		}
	}
}

// Snapshot returns entries in chronological order; when from/to are non-nil
// (and valid per .IsValid()), only entries with from <= At <= to are returned.
func (h *logHistory) Snapshot(from, to *timestamppb.Timestamp) []*observabilityv0.Log {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.snapshotLocked(from, to)
}

func (h *logHistory) snapshotLocked(from, to *timestamppb.Timestamp) []*observabilityv0.Log {
	var ordered []*observabilityv0.Log
	if h.full {
		ordered = append(ordered, h.buf[h.next:]...)
		ordered = append(ordered, h.buf[:h.next]...)
	} else {
		ordered = append(ordered, h.buf[:h.next]...)
	}

	hasFrom := from != nil && from.IsValid()
	hasTo := to != nil && to.IsValid()
	if !hasFrom && !hasTo {
		return ordered
	}

	var out []*observabilityv0.Log
	for _, entry := range ordered {
		if hasFrom && entry.At.AsTime().Before(from.AsTime()) {
			continue
		}
		if hasTo && entry.At.AsTime().After(to.AsTime()) {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// Subscribe atomically returns the current history snapshot plus a channel
// that receives every entry Added after this call returns. The caller MUST
// call the returned unsubscribe func exactly once when done, to stop future
// Add calls from blocking on (or leaking into) an abandoned channel.
func (h *logHistory) Subscribe(capacity int) (snapshot []*observabilityv0.Log, live <-chan *observabilityv0.Log, unsubscribe func()) {
	h.mu.Lock()
	defer h.mu.Unlock()

	snapshot = h.snapshotLocked(nil, nil)
	ch := make(chan *observabilityv0.Log, capacity)
	id := h.nextSubID
	h.nextSubID++
	h.subscribers[id] = ch

	return snapshot, ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		delete(h.subscribers, id)
	}
}
