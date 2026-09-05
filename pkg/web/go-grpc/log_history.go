package go_grpc

import (
	"sync"

	"google.golang.org/protobuf/types/known/timestamppb"

	observabilityv0 "github.com/codefly-dev/core/generated/go/codefly/observability/v0"
)

// logHistory is a fixed-capacity ring buffer of the most recent log entries,
// so a dashboard opened (or reloaded) after a run started can backfill.
type logHistory struct {
	mu   sync.Mutex
	buf  []*observabilityv0.Log
	next int
	full bool
}

func newLogHistory(capacity int) *logHistory {
	return &logHistory{buf: make([]*observabilityv0.Log, capacity)}
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
}

// Snapshot returns entries in chronological order; when from/to are non-nil
// (and valid per .IsValid()), only entries with from <= At <= to are returned.
func (h *logHistory) Snapshot(from, to *timestamppb.Timestamp) []*observabilityv0.Log {
	h.mu.Lock()
	defer h.mu.Unlock()

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
