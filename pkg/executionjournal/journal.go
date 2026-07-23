// Package executionjournal provides Codefly's durable, product-neutral
// execution-attestation outbox. It knows nothing about Warden or any other
// exporter product.
package executionjournal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/codefly-dev/core/executionreceipt"
	executionv1 "github.com/codefly-dev/core/generated/go/codefly/execution/v1"
	bolt "go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"
)

const (
	schemaVersion    = 1
	maxPendingLimit  = 1000
	maxRecordBytes   = 256 * 1024
	defaultOpenDelay = 500 * time.Millisecond
)

var (
	// ErrInvalid is returned when journal input is malformed.
	ErrInvalid = errors.New("invalid Codefly execution journal input")
	// ErrConflict is returned when an immutable receipt, stage, or attempt
	// binding is reused with different facts.
	ErrConflict = errors.New("Codefly execution journal conflict")
	// ErrClosed is returned after the journal has been closed.
	ErrClosed = errors.New("Codefly execution journal is closed")

	metaBucket           = []byte("meta")
	recordsBucket        = []byte("records")
	receiptIndexBucket   = []byte("receipt-index")
	stageIndexBucket     = []byte("stage-index")
	attemptBindingBucket = []byte("attempt-binding")
	ackBucket            = []byte("ack")
	schemaVersionKey     = []byte("schema-version")
	nextSequenceKey      = []byte("next-sequence")
)

// Verifier proves that an attestation was signed by a currently trusted
// Gateway key. The journal never accepts unsigned or merely well-shaped data.
type Verifier func(*executionv1.ExecutionAttestationV1) error

// Entry is one durable ordered attestation.
type Entry struct {
	Sequence    uint64
	Attestation *executionv1.ExecutionAttestationV1
}

// AppendResult reports the stable sequence and idempotent duplicate status.
type AppendResult struct {
	Sequence  uint64
	Duplicate bool
}

// AckResult reports whether this exact acknowledgement already existed.
type AckResult struct {
	Duplicate bool
}

// Journal is a single-process BoltDB-backed durable outbox. BoltDB owns the
// inter-process file lock and fsync-backed transaction boundary.
type Journal struct {
	mu       sync.RWMutex
	db       *bolt.DB
	verifier Verifier
}

// Open creates or opens an owner-only journal. Existing symlinks or
// group/world-accessible state fail closed.
func Open(ctx context.Context, path string, verifier Verifier) (*Journal, error) {
	if verifier == nil {
		return nil, fmt.Errorf("%w: verifier is required", ErrInvalid)
	}
	path = filepath.Clean(path)
	if path == "." || path == string(filepath.Separator) {
		return nil, fmt.Errorf("%w: journal path is required", ErrInvalid)
	}
	if err := ensurePrivateDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	}
	if err := rejectUnsafeFile(path); err != nil {
		return nil, err
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: defaultOpenDelay})
	if err != nil {
		return nil, fmt.Errorf("open execution journal: %w", err)
	}
	journal := &Journal{db: db, verifier: verifier}
	if err := journal.initialize(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := rejectUnsafeFile(path); err != nil {
		_ = db.Close()
		return nil, err
	}
	return journal, nil
}

func (j *Journal) initialize(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := j.db.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{
			metaBucket, recordsBucket, receiptIndexBucket, stageIndexBucket,
			attemptBindingBucket, ackBucket,
		} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		meta := tx.Bucket(metaBucket)
		version := meta.Get(schemaVersionKey)
		if version == nil {
			var encoded [8]byte
			binary.BigEndian.PutUint64(encoded[:], schemaVersion)
			if err := meta.Put(schemaVersionKey, encoded[:]); err != nil {
				return err
			}
		} else if len(version) != 8 || binary.BigEndian.Uint64(version) != schemaVersion {
			return fmt.Errorf("unsupported execution journal schema version")
		}
		return nil
	}); err != nil {
		return fmt.Errorf("initialize execution journal: %w", err)
	}
	if err := j.db.View(func(tx *bolt.Tx) error {
		for checkErr := range tx.Check() {
			if checkErr != nil {
				return checkErr
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("execution journal integrity check: %w", err)
	}
	return nil
}

// Append durably adds one attestation. The transaction commits before Append
// returns, so callers may use a STARTED append as the pre-effect boundary.
func (j *Journal) Append(ctx context.Context, attestation *executionv1.ExecutionAttestationV1) (AppendResult, error) {
	if j == nil {
		return AppendResult{}, ErrClosed
	}
	j.mu.RLock()
	defer j.mu.RUnlock()
	if err := j.usable(ctx); err != nil {
		return AppendResult{}, err
	}
	if err := j.verifier(attestation); err != nil {
		return AppendResult{}, fmt.Errorf("%w: verify attestation: %v", ErrInvalid, err)
	}
	prepared, err := executionreceipt.Prepare(attestation.GetReceipt())
	if err != nil {
		return AppendResult{}, err
	}
	if !proto.Equal(prepared, attestation.GetReceipt()) {
		return AppendResult{}, fmt.Errorf("%w: receipt is not in prepared canonical form", ErrInvalid)
	}
	payload, err := (proto.MarshalOptions{Deterministic: true}).Marshal(attestation)
	if err != nil {
		return AppendResult{}, fmt.Errorf("%w: marshal attestation: %v", ErrInvalid, err)
	}
	if len(payload) == 0 || len(payload) > maxRecordBytes {
		return AppendResult{}, fmt.Errorf("%w: attestation is %d bytes; maximum is %d", ErrInvalid, len(payload), maxRecordBytes)
	}
	recordHash := sha256.Sum256(payload)
	receipt := prepared
	attemptKey, bindingHash, err := attemptIdentity(receipt)
	if err != nil {
		return AppendResult{}, err
	}
	stageKey := appendStage(attemptKey, receipt.GetStage())

	var result AppendResult
	err = j.db.Update(func(tx *bolt.Tx) error {
		index := tx.Bucket(receiptIndexBucket)
		if existing := index.Get([]byte(receipt.GetReceiptId())); existing != nil {
			sequence, hash, decodeErr := decodeIndex(existing)
			if decodeErr != nil {
				return decodeErr
			}
			if bytes.Equal(hash, recordHash[:]) {
				result = AppendResult{Sequence: sequence, Duplicate: true}
				return nil
			}
			return fmt.Errorf("%w: receipt_id %q has different immutable bytes", ErrConflict, receipt.GetReceiptId())
		}
		stages := tx.Bucket(stageIndexBucket)
		if existing := stages.Get(stageKey); existing != nil {
			return fmt.Errorf("%w: attempt already has stage %s under receipt %q",
				ErrConflict, receipt.GetStage(), existing)
		}
		if hasTerminal(stages, attemptKey) {
			return fmt.Errorf("%w: attempt already has a terminal stage", ErrConflict)
		}
		if receipt.GetStage() == executionv1.ExecutionStage_EXECUTION_STAGE_ADMITTED {
			startedKey := appendStage(attemptKey, executionv1.ExecutionStage_EXECUTION_STAGE_STARTED)
			if stages.Get(startedKey) != nil {
				return fmt.Errorf("%w: ADMITTED cannot be appended after STARTED", ErrConflict)
			}
		}
		if terminal(receipt.GetStage()) {
			startedKey := appendStage(attemptKey, executionv1.ExecutionStage_EXECUTION_STAGE_STARTED)
			if stages.Get(startedKey) == nil {
				return fmt.Errorf("%w: terminal stage requires a durable STARTED receipt", ErrConflict)
			}
		}
		bindings := tx.Bucket(attemptBindingBucket)
		if existing := bindings.Get(attemptKey); existing != nil {
			if !bytes.Equal(existing, bindingHash[:]) {
				return fmt.Errorf("%w: attempt binding changed between stages", ErrConflict)
			}
		} else if err := bindings.Put(attemptKey, bindingHash[:]); err != nil {
			return err
		}

		sequence, err := nextSequence(tx.Bucket(metaBucket))
		if err != nil {
			return err
		}
		sequenceKey := encodeSequence(sequence)
		if err := tx.Bucket(recordsBucket).Put(sequenceKey, payload); err != nil {
			return err
		}
		if err := index.Put([]byte(receipt.GetReceiptId()), encodeIndex(sequence, recordHash)); err != nil {
			return err
		}
		if err := stages.Put(stageKey, []byte(receipt.GetReceiptId())); err != nil {
			return err
		}
		result = AppendResult{Sequence: sequence}
		return nil
	})
	if err != nil {
		return AppendResult{}, fmt.Errorf("append execution attestation: %w", err)
	}
	return result, nil
}

// Pending returns unacknowledged attestations in sequence order. afterSequence
// is an optimization hint, never authority to skip a gap: any older
// unacknowledged receipt is returned first. Exporters persist a cursor only
// after acknowledging every preceding receipt.
func (j *Journal) Pending(ctx context.Context, afterSequence uint64, limit int) ([]Entry, error) {
	if j == nil {
		return nil, ErrClosed
	}
	j.mu.RLock()
	defer j.mu.RUnlock()
	if err := j.usable(ctx); err != nil {
		return nil, err
	}
	if limit < 1 || limit > maxPendingLimit {
		return nil, fmt.Errorf("%w: pending limit must be between 1 and %d", ErrInvalid, maxPendingLimit)
	}
	var entries []Entry
	err := j.db.View(func(tx *bolt.Tx) error {
		cursor := tx.Bucket(recordsBucket).Cursor()
		acks := tx.Bucket(ackBucket)
		startKey, _ := cursor.First()
		if afterSequence > 0 {
			startKey = nil
			for key, value := cursor.First(); key != nil; key, value = cursor.Next() {
				sequence, err := decodeSequence(key)
				if err != nil {
					return err
				}
				if sequence > afterSequence {
					startKey = key
					break
				}
				attestation := &executionv1.ExecutionAttestationV1{}
				if err := proto.Unmarshal(value, attestation); err != nil {
					return fmt.Errorf("decode sequence %d: %w", sequence, err)
				}
				if acks.Get([]byte(attestation.GetReceipt().GetReceiptId())) == nil {
					// The exporter cursor moved past a durable gap. Resume at
					// the oldest unacknowledged receipt, never after it.
					startKey = key
					break
				}
			}
		}
		for key, value := cursor.Seek(startKey); key != nil && len(entries) < limit; key, value = cursor.Next() {
			sequence, err := decodeSequence(key)
			if err != nil {
				return err
			}
			attestation := &executionv1.ExecutionAttestationV1{}
			if err := proto.Unmarshal(value, attestation); err != nil {
				return fmt.Errorf("decode sequence %d: %w", sequence, err)
			}
			if acks.Get([]byte(attestation.GetReceipt().GetReceiptId())) != nil {
				continue
			}
			entries = append(entries, Entry{Sequence: sequence, Attestation: attestation})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read pending execution attestations: %w", err)
	}
	return entries, nil
}

// Acknowledge durably marks the exact receipt digest accepted by an exporter.
func (j *Journal) Acknowledge(ctx context.Context, receiptID, payloadSHA256 string) (AckResult, error) {
	if j == nil {
		return AckResult{}, ErrClosed
	}
	j.mu.RLock()
	defer j.mu.RUnlock()
	if err := j.usable(ctx); err != nil {
		return AckResult{}, err
	}
	if receiptID == "" || payloadSHA256 == "" {
		return AckResult{}, fmt.Errorf("%w: receipt ID and payload digest are required", ErrInvalid)
	}
	var result AckResult
	err := j.db.Update(func(tx *bolt.Tx) error {
		indexValue := tx.Bucket(receiptIndexBucket).Get([]byte(receiptID))
		if indexValue == nil {
			return fmt.Errorf("%w: unknown receipt_id %q", ErrInvalid, receiptID)
		}
		sequence, _, err := decodeIndex(indexValue)
		if err != nil {
			return err
		}
		record := tx.Bucket(recordsBucket).Get(encodeSequence(sequence))
		if record == nil {
			return fmt.Errorf("receipt index points to missing sequence %d", sequence)
		}
		attestation := &executionv1.ExecutionAttestationV1{}
		if err := proto.Unmarshal(record, attestation); err != nil {
			return err
		}
		if attestation.GetReceipt().GetPayloadSha256() != payloadSHA256 {
			return fmt.Errorf("%w: acknowledgement digest does not match receipt", ErrConflict)
		}
		acks := tx.Bucket(ackBucket)
		if existing := acks.Get([]byte(receiptID)); existing != nil {
			result.Duplicate = true
			return nil
		}
		return acks.Put([]byte(receiptID), encodeSequence(sequence))
	})
	if err != nil {
		return AckResult{}, fmt.Errorf("acknowledge execution attestation: %w", err)
	}
	return result, nil
}

// IncompleteAttempts returns durable STARTED receipts that have no terminal
// stage. On restart the Gateway signs and appends an UNCERTAIN terminal receipt
// for each entry before accepting new work.
func (j *Journal) IncompleteAttempts(ctx context.Context, limit int) ([]Entry, error) {
	if j == nil {
		return nil, ErrClosed
	}
	j.mu.RLock()
	defer j.mu.RUnlock()
	if err := j.usable(ctx); err != nil {
		return nil, err
	}
	if limit < 1 || limit > maxPendingLimit {
		return nil, fmt.Errorf("%w: incomplete limit must be between 1 and %d", ErrInvalid, maxPendingLimit)
	}
	var entries []Entry
	err := j.db.View(func(tx *bolt.Tx) error {
		stages := tx.Bucket(stageIndexBucket)
		cursor := stages.Cursor()
		for key, receiptID := cursor.First(); key != nil && len(entries) < limit; key, receiptID = cursor.Next() {
			attemptKey, stage, err := splitStageKey(key)
			if err != nil {
				return err
			}
			if stage != executionv1.ExecutionStage_EXECUTION_STAGE_STARTED || hasTerminal(stages, attemptKey) {
				continue
			}
			entry, err := entryByReceiptID(tx, string(receiptID))
			if err != nil {
				return err
			}
			entries = append(entries, entry)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read incomplete execution attempts: %w", err)
	}
	return entries, nil
}

// PruneAcknowledgedThrough removes only complete attempts for which every
// receipt is acknowledged and whose last sequence is at or below through.
// Unacknowledged or incomplete attempts are never eligible.
func (j *Journal) PruneAcknowledgedThrough(ctx context.Context, through uint64) (int, error) {
	if j == nil {
		return 0, ErrClosed
	}
	j.mu.RLock()
	defer j.mu.RUnlock()
	if err := j.usable(ctx); err != nil {
		return 0, err
	}
	pruned := 0
	err := j.db.Update(func(tx *bolt.Tx) error {
		type attemptRecords struct {
			terminal bool
			eligible bool
			entries  []Entry
		}
		attempts := make(map[string]*attemptRecords)
		cursor := tx.Bucket(recordsBucket).Cursor()
		for key, value := cursor.First(); key != nil; key, value = cursor.Next() {
			sequence, err := decodeSequence(key)
			if err != nil {
				return err
			}
			attestation := &executionv1.ExecutionAttestationV1{}
			if err := proto.Unmarshal(value, attestation); err != nil {
				return err
			}
			attemptKey, _, err := attemptIdentity(attestation.GetReceipt())
			if err != nil {
				return err
			}
			id := string(attemptKey)
			group := attempts[id]
			if group == nil {
				group = &attemptRecords{eligible: true}
				attempts[id] = group
			}
			group.entries = append(group.entries, Entry{Sequence: sequence, Attestation: attestation})
			group.terminal = group.terminal || terminal(attestation.GetReceipt().GetStage())
			if sequence > through || tx.Bucket(ackBucket).Get([]byte(attestation.GetReceipt().GetReceiptId())) == nil {
				group.eligible = false
			}
		}
		for id, group := range attempts {
			if !group.eligible || !group.terminal {
				continue
			}
			attemptKey := []byte(id)
			for _, entry := range group.entries {
				receipt := entry.Attestation.GetReceipt()
				if err := tx.Bucket(recordsBucket).Delete(encodeSequence(entry.Sequence)); err != nil {
					return err
				}
				if err := tx.Bucket(receiptIndexBucket).Delete([]byte(receipt.GetReceiptId())); err != nil {
					return err
				}
				if err := tx.Bucket(ackBucket).Delete([]byte(receipt.GetReceiptId())); err != nil {
					return err
				}
				if err := tx.Bucket(stageIndexBucket).Delete(appendStage(attemptKey, receipt.GetStage())); err != nil {
					return err
				}
				pruned++
			}
			if err := tx.Bucket(attemptBindingBucket).Delete(attemptKey); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("prune execution journal: %w", err)
	}
	return pruned, nil
}

// Close flushes and releases the journal's process lock.
func (j *Journal) Close() error {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.db == nil {
		return nil
	}
	db := j.db
	j.db = nil
	return db.Close()
}

func (j *Journal) usable(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if j == nil || j.db == nil {
		return ErrClosed
	}
	return nil
}

func ensurePrivateDirectory(path string) error {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("create execution journal directory: %w", err)
		}
		info, err = os.Lstat(path)
	case err != nil:
		return fmt.Errorf("inspect execution journal directory: %w", err)
	}
	if err != nil {
		return fmt.Errorf("inspect execution journal directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%w: execution journal parent must be a real directory", ErrInvalid)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: execution journal directory permissions %04o are not owner-only", ErrInvalid, info.Mode().Perm())
	}
	return nil
}

func rejectUnsafeFile(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect execution journal file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("%w: execution journal must be a regular file", ErrInvalid)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: execution journal permissions %04o are not owner-only", ErrInvalid, info.Mode().Perm())
	}
	pageSize := int64(os.Getpagesize())
	if info.Size() != 0 && (info.Size() < 2*pageSize || info.Size()%pageSize != 0) {
		return fmt.Errorf("%w: execution journal size %d is not a complete BoltDB page set", ErrInvalid, info.Size())
	}
	return nil
}

func nextSequence(meta *bolt.Bucket) (uint64, error) {
	current := uint64(0)
	if encoded := meta.Get(nextSequenceKey); encoded != nil {
		if len(encoded) != 8 {
			return 0, errors.New("invalid next sequence metadata")
		}
		current = binary.BigEndian.Uint64(encoded)
	}
	if current == ^uint64(0) {
		return 0, errors.New("execution journal sequence exhausted")
	}
	next := current + 1
	if err := meta.Put(nextSequenceKey, encodeSequence(next)); err != nil {
		return 0, err
	}
	return next, nil
}

func encodeSequence(sequence uint64) []byte {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], sequence)
	return encoded[:]
}

func decodeSequence(encoded []byte) (uint64, error) {
	if len(encoded) != 8 {
		return 0, errors.New("invalid execution journal sequence")
	}
	return binary.BigEndian.Uint64(encoded), nil
}

func encodeIndex(sequence uint64, hash [sha256.Size]byte) []byte {
	encoded := make([]byte, 8+sha256.Size)
	binary.BigEndian.PutUint64(encoded, sequence)
	copy(encoded[8:], hash[:])
	return encoded
}

func decodeIndex(encoded []byte) (uint64, []byte, error) {
	if len(encoded) != 8+sha256.Size {
		return 0, nil, errors.New("invalid execution receipt index")
	}
	return binary.BigEndian.Uint64(encoded[:8]), encoded[8:], nil
}

func attemptIdentity(receipt *executionv1.ExecutionReceiptV1) ([]byte, [sha256.Size]byte, error) {
	if receipt == nil || receipt.GetWorkContext() == nil || receipt.GetProducer() == nil {
		return nil, [sha256.Size]byte{}, fmt.Errorf("%w: receipt identity is incomplete", ErrInvalid)
	}
	keyHash := sha256.New()
	for _, value := range []string{
		receipt.GetWorkContext().GetTenantId(),
		receipt.GetProducer().GetId(),
		receipt.GetOperationId(),
		receipt.GetAttemptId(),
	} {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(value)))
		_, _ = keyHash.Write(length[:])
		_, _ = keyHash.Write([]byte(value))
	}
	attemptKey := keyHash.Sum(nil)

	binding := proto.Clone(receipt).(*executionv1.ExecutionReceiptV1)
	binding.ReceiptId = ""
	binding.Stage = executionv1.ExecutionStage_EXECUTION_STAGE_UNSPECIFIED
	binding.CompletedAt = nil
	binding.Resources = nil
	binding.Result = nil
	binding.ExtensionSchema = nil
	binding.ExtensionJson = nil
	binding.PayloadSha256 = ""
	payload, err := (proto.MarshalOptions{Deterministic: true}).Marshal(binding)
	if err != nil {
		return nil, [sha256.Size]byte{}, fmt.Errorf("%w: marshal attempt binding: %v", ErrInvalid, err)
	}
	return attemptKey, sha256.Sum256(payload), nil
}

func appendStage(attemptKey []byte, stage executionv1.ExecutionStage) []byte {
	key := make([]byte, len(attemptKey)+1)
	copy(key, attemptKey)
	key[len(attemptKey)] = byte(stage)
	return key
}

func splitStageKey(key []byte) ([]byte, executionv1.ExecutionStage, error) {
	if len(key) != sha256.Size+1 {
		return nil, executionv1.ExecutionStage_EXECUTION_STAGE_UNSPECIFIED, errors.New("invalid execution stage index key")
	}
	return key[:sha256.Size], executionv1.ExecutionStage(key[sha256.Size]), nil
}

func terminal(stage executionv1.ExecutionStage) bool {
	switch stage {
	case executionv1.ExecutionStage_EXECUTION_STAGE_SUCCEEDED,
		executionv1.ExecutionStage_EXECUTION_STAGE_FAILED,
		executionv1.ExecutionStage_EXECUTION_STAGE_COMPENSATED,
		executionv1.ExecutionStage_EXECUTION_STAGE_UNCERTAIN:
		return true
	default:
		return false
	}
}

func hasTerminal(stages *bolt.Bucket, attemptKey []byte) bool {
	for _, stage := range []executionv1.ExecutionStage{
		executionv1.ExecutionStage_EXECUTION_STAGE_SUCCEEDED,
		executionv1.ExecutionStage_EXECUTION_STAGE_FAILED,
		executionv1.ExecutionStage_EXECUTION_STAGE_COMPENSATED,
		executionv1.ExecutionStage_EXECUTION_STAGE_UNCERTAIN,
	} {
		if stages.Get(appendStage(attemptKey, stage)) != nil {
			return true
		}
	}
	return false
}

func entryByReceiptID(tx *bolt.Tx, receiptID string) (Entry, error) {
	index := tx.Bucket(receiptIndexBucket).Get([]byte(receiptID))
	if index == nil {
		return Entry{}, fmt.Errorf("stage index points to missing receipt %q", receiptID)
	}
	sequence, _, err := decodeIndex(index)
	if err != nil {
		return Entry{}, err
	}
	record := tx.Bucket(recordsBucket).Get(encodeSequence(sequence))
	if record == nil {
		return Entry{}, fmt.Errorf("receipt %q points to missing sequence %d", receiptID, sequence)
	}
	attestation := &executionv1.ExecutionAttestationV1{}
	if err := proto.Unmarshal(record, attestation); err != nil {
		return Entry{}, err
	}
	return Entry{Sequence: sequence, Attestation: attestation}, nil
}
