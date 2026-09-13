package ci

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/codefly-dev/core/resources"
	"github.com/spf13/cobra"
)

const (
	ciResultRecordSchema = "codefly.ci-result/v1"
	ciResultOutcomePass  = "passed"
	ciResultSignatureAlg = "hmac-sha256"
	// ciResultKeyVariable carries the signing key out of band. Only runs on a
	// protected reference may hold it: a writer without it cannot publish a
	// record any verifier will accept, whatever access it has to the backend.
	ciResultKeyVariable = "CODEFLY_CI_RESULT_KEY"
)

// ciResultRecord is one authenticated successful task execution. It is the unit
// a later run may stand in for, and it is self-describing: a verifier needs the
// trust policy and the signing key, never the storage backend's cooperation.
type ciResultRecord struct {
	Schema         string           `json:"schema"`
	IdentitySchema int              `json:"identity_schema_version"`
	Identity       string           `json:"identity"`
	Task           string           `json:"task"`
	Phase          string           `json:"phase"`
	Suite          string           `json:"suite,omitempty"`
	Service        string           `json:"service,omitempty"`
	Outcome        string           `json:"outcome"`
	Reference      string           `json:"reference"`
	Run            string           `json:"run"`
	Revision       string           `json:"revision,omitempty"`
	Environment    string           `json:"environment"`
	RecordedAt     string           `json:"recorded_at"`
	Evidence       ciResultEvidence `json:"evidence"`
	Signature      string           `json:"signature"`
}

type ciResultEvidence struct {
	Audit     *CIReportAudit     `json:"audit,omitempty"`
	Drift     *CIReportDrift     `json:"drift,omitempty"`
	Integrity *CIReportIntegrity `json:"integrity,omitempty"`
	Artifacts []CIReportArtifact `json:"artifacts"`
}

func (record *ciResultRecord) sign(key []byte) (string, error) {
	unsigned := *record
	unsigned.Signature = ""
	payload, err := json.Marshal(&unsigned)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(payload)
	return ciResultSignatureAlg + ":" + hex.EncodeToString(mac.Sum(nil)), nil
}

func (record *ciResultRecord) authentic(key []byte) bool {
	expected, err := record.sign(key)
	if err != nil {
		return false
	}
	return hmac.Equal([]byte(expected), []byte(record.Signature))
}

// ciResultStore is a content-addressed directory of records and artifact blobs.
// A record is published only after every blob it names is durable, and each
// publication is a single rename, so a concurrent reader sees the previous
// record or the complete new one.
type ciResultStore struct {
	root string
}

func (store *ciResultStore) recordPath(identity string) string {
	return filepath.Join(store.root, "records", hex.EncodeToString(sha256Sum([]byte(identity)))+".json")
}

func (store *ciResultStore) blobPath(digest string) (string, error) {
	hexDigest := strings.TrimPrefix(digest, "sha256:")
	if len(hexDigest) != 64 {
		return "", fmt.Errorf("artifact digest %q is not a SHA-256 identity", digest)
	}
	if _, err := hex.DecodeString(hexDigest); err != nil {
		return "", fmt.Errorf("artifact digest %q is not hexadecimal", digest)
	}
	return filepath.Join(store.root, "blobs", "sha256", hexDigest), nil
}

func (store *ciResultStore) lookup(identity string) (*ciResultRecord, error) {
	payload, err := os.ReadFile(store.recordPath(identity))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var record ciResultRecord
	if err := json.Unmarshal(payload, &record); err != nil {
		return nil, fmt.Errorf("decode result record: %w", err)
	}
	return &record, nil
}

func (store *ciResultStore) blob(digest string) ([]byte, error) {
	path, err := store.blobPath(digest)
	if err != nil {
		return nil, err
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if artifactDigest(payload) != digest {
		return nil, fmt.Errorf("stored artifact %s does not match its digest", digest)
	}
	return payload, nil
}

func (store *ciResultStore) publish(record *ciResultRecord, blobs map[string][]byte) error {
	for digest, payload := range blobs {
		if artifactDigest(payload) != digest {
			return fmt.Errorf("refusing to publish artifact that does not match digest %s", digest)
		}
		path, err := store.blobPath(digest)
		if err != nil {
			return err
		}
		if err := writeCIReportAtomic(path, payload); err != nil {
			return err
		}
	}
	payload, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return writeCIReportAtomic(store.recordPath(record.Identity), append(payload, '\n'))
}

func sha256Sum(payload []byte) []byte {
	sum := sha256.Sum256(payload)
	return sum[:]
}

func artifactDigest(payload []byte) string {
	return "sha256:" + hex.EncodeToString(sha256Sum(payload))
}

// ciReuseFlags is the operator-owned trust and storage policy. Nothing here is
// derived from a cached record, so a record cannot widen its own trust scope.
type ciReuseFlags struct {
	enabled             bool
	store               string
	environment         string
	reference           string
	trustedReferences   []string
	run                 string
	maxAge              time.Duration
	timeSensitiveMaxAge time.Duration
}

var ciReuse ciReuseFlags

func bindReuseFlags(command *cobra.Command) {
	command.Flags().BoolVar(&ciReuse.enabled, "reuse-results", false, "Reuse verified successful task results from a trusted reference instead of re-executing identical work")
	command.Flags().StringVar(&ciReuse.store, "reuse-store", "", "Directory holding verified CI result records and artifacts (default $CODEFLY_CI_RESULT_STORE)")
	command.Flags().StringVar(&ciReuse.environment, "reuse-environment", "", "Identity of the execution environment (for example the runner image digest; default $CODEFLY_CI_REUSE_ENVIRONMENT)")
	command.Flags().StringVar(&ciReuse.reference, "reuse-reference", "", "Reference this run publishes results under (default $CODEFLY_CI_REFERENCE)")
	command.Flags().StringSliceVar(&ciReuse.trustedReferences, "reuse-trusted-reference", nil, "Reference whose successful results may be reused (repeatable; required with --reuse-results)")
	command.Flags().StringVar(&ciReuse.run, "reuse-run", "", "Identity of this run, recorded as the provenance of published results (default $CODEFLY_CI_RUN)")
	command.Flags().DurationVar(&ciReuse.maxAge, "reuse-max-age", 168*time.Hour, "Maximum age of a reusable result")
	command.Flags().DurationVar(&ciReuse.timeSensitiveMaxAge, "reuse-audit-max-age", 0, "Maximum age of a reusable dependency-audit result; zero always re-runs audits because advisory data changes independently of source")
}

// ciResultReuse decides, for one run, whether a task may stand on a previously
// verified execution. Every rejection keeps the task executing; nothing here can
// turn a missing, unreadable or untrusted record into a success.
type ciResultReuse struct {
	store               *ciResultStore
	key                 []byte
	reference           string
	trusted             map[string]bool
	run                 string
	revision            string
	environment         string
	maxAge              time.Duration
	timeSensitiveMaxAge time.Duration
	outputDirectory     string
	now                 func() time.Time
}

func newCIResultReuse(ctx context.Context, workspace *resources.Workspace, flags *ciReuseFlags) (*ciResultReuse, error) {
	if !flags.enabled {
		return nil, nil
	}
	store := firstNonEmpty(flags.store, os.Getenv("CODEFLY_CI_RESULT_STORE"))
	if store == "" {
		return nil, fmt.Errorf("--reuse-results requires --reuse-store or CODEFLY_CI_RESULT_STORE")
	}
	environment := firstNonEmpty(flags.environment, os.Getenv("CODEFLY_CI_REUSE_ENVIRONMENT"))
	if environment == "" {
		return nil, fmt.Errorf("--reuse-results requires --reuse-environment or CODEFLY_CI_REUSE_ENVIRONMENT: an unidentified environment cannot be matched")
	}
	trusted := map[string]bool{}
	for _, reference := range sortedUnique(flags.trustedReferences) {
		trusted[reference] = true
	}
	if len(trusted) == 0 {
		return nil, fmt.Errorf("--reuse-results requires --reuse-trusted-reference: reuse has no trust scope otherwise")
	}
	key := strings.TrimSpace(os.Getenv(ciResultKeyVariable))
	if key == "" {
		return nil, fmt.Errorf("--reuse-results requires %s: unauthenticated records cannot certify success", ciResultKeyVariable)
	}
	reuse := &ciResultReuse{
		store:               &ciResultStore{root: cleanAbs(store)},
		key:                 []byte(key),
		reference:           firstNonEmpty(flags.reference, os.Getenv("CODEFLY_CI_REFERENCE")),
		trusted:             trusted,
		run:                 firstNonEmpty(flags.run, os.Getenv("CODEFLY_CI_RUN")),
		environment:         environment,
		maxAge:              flags.maxAge,
		timeSensitiveMaxAge: flags.timeSensitiveMaxAge,
		outputDirectory:     resolveCIOutputDirectory(workspace, ciReportOutput),
		now:                 time.Now,
	}
	if reuse.publishes() && reuse.run == "" {
		return nil, fmt.Errorf("publishing reusable results requires --reuse-run or CODEFLY_CI_RUN for successful execution provenance")
	}
	if root, err := gitRoot(ctx, workspace.Dir()); err == nil {
		if revision, revErr := gitOutput(ctx, root, "rev-parse", "HEAD^{commit}"); revErr == nil {
			reuse.revision = strings.TrimSpace(string(revision))
		}
	}
	return reuse, nil
}

// publishes reports whether this run is itself allowed to write results. A run
// on an untrusted reference consumes evidence but never produces it, so a
// pull-request run cannot seed the records a protected reference is verified
// against even when it can reach the same backend.
func (reuse *ciResultReuse) publishes() bool {
	return reuse.trusted[reuse.reference]
}

// reuseTimeSensitive marks phases whose correct answer depends on data that
// changes independently of the inputs an identity binds.
func reuseTimeSensitive(phase string) bool {
	return phase == ciPhaseAudit
}

// reuseVerifiableOutputs reports whether Codefly records every output a phase
// produces. A build publishes container images through its agent, which the
// report neither enumerates nor restores, so a hit would release a downstream
// task against artifacts that are not there.
func reuseVerifiableOutputs(phase string) bool {
	return phase != string(resources.PhaseBuild)
}

func (reuse *ciResultReuse) freshness(phase string) time.Duration {
	if reuseTimeSensitive(phase) {
		return reuse.timeSensitiveMaxAge
	}
	return reuse.maxAge
}

// ciReuseDecision is what this run concluded about one task's cache entry.
// ineligible means the task's own identity or the reuse policy forbids standing
// on any record; miss means no usable record was found.
type ciReuseDecision struct {
	record *ciResultRecord
	status string
	reason string
}

func ineligibleReuse(reason string) ciReuseDecision {
	return ciReuseDecision{status: cacheStatusIneligible, reason: reason}
}

func missedReuse(reason string) ciReuseDecision {
	return ciReuseDecision{status: cacheStatusMiss, reason: reason}
}

// lookup returns the verified record a task may stand on, or why it may not.
// Artifacts are not restored here: nothing is written to the workspace until the
// record itself has been accepted.
func (reuse *ciResultReuse) lookup(identity *CICacheIdentity, phase string) ciReuseDecision {
	if eligible, reason := identity.reuseEligibility(); !eligible {
		return ineligibleReuse(reason)
	}
	if !reuseVerifiableOutputs(phase) {
		return ineligibleReuse("results for this phase are never reused: " + phase + " produces artifacts Codefly does not record or restore")
	}
	freshness := reuse.freshness(phase)
	if freshness <= 0 {
		return ineligibleReuse("results for this phase are never reused: " + phase + " depends on data that changes independently of its inputs")
	}
	record, err := reuse.store.lookup(identity.Key)
	if err != nil {
		return missedReuse("result record cannot be read: " + err.Error())
	}
	if record == nil {
		return missedReuse("no verified result was recorded for this identity")
	}
	switch {
	case record.Schema != ciResultRecordSchema:
		return missedReuse("result record schema is incompatible")
	case record.IdentitySchema != cacheIdentitySchemaVersion:
		return missedReuse("result record was written against a different identity contract")
	case record.Identity != identity.Key:
		return missedReuse("result record does not match the requested identity")
	case !record.authentic(reuse.key):
		return missedReuse("result record is not authentic")
	case record.Outcome != ciResultOutcomePass:
		return missedReuse("recorded result is not a success")
	case !reuse.trusted[record.Reference]:
		return missedReuse("result was produced on untrusted reference " + record.Reference)
	case strings.TrimSpace(record.Run) == "":
		return missedReuse("result record has no producing run")
	case record.Task != reportTaskID(phase, identity.Inputs.Suite, identity.Inputs.Service) ||
		record.Phase != phase || record.Service != identity.Inputs.Service ||
		normalizedCacheSuite(record.Phase, record.Suite) != identity.Inputs.Suite:
		return missedReuse("result record task does not match the requested task")
	case record.Environment != reuse.environment:
		return missedReuse("result was produced in a different execution environment")
	}
	recordedAt, err := time.Parse(time.RFC3339Nano, record.RecordedAt)
	if err != nil {
		return missedReuse("result record has no readable success time")
	}
	age := reuse.now().Sub(recordedAt)
	if age < 0 {
		return missedReuse("result record success time is in the future")
	}
	if age > freshness {
		return missedReuse("recorded result is older than the configured reuse window")
	}
	return ciReuseDecision{record: record, status: cacheStatusHit}
}

// restore materializes every artifact a reused task would otherwise have
// produced and verifies each one on disk, so a downstream task consuming a hit
// reads the same bytes the original execution wrote.
func (reuse *ciResultReuse) restore(record *ciResultRecord) error {
	for _, artifact := range record.Evidence.Artifacts {
		payload, err := reuse.store.blob(artifact.SHA256)
		if err != nil {
			return fmt.Errorf("restore artifact %s: %w", artifact.Path, err)
		}
		destination, err := reuse.artifactPath(artifact.Path)
		if err != nil {
			return err
		}
		if writeErr := writeCIReportAtomic(destination, payload); writeErr != nil {
			return fmt.Errorf("restore artifact %s: %w", artifact.Path, writeErr)
		}
		written, err := os.ReadFile(destination)
		if err != nil {
			return fmt.Errorf("verify restored artifact %s: %w", artifact.Path, err)
		}
		if artifactDigest(written) != artifact.SHA256 {
			return fmt.Errorf("restored artifact %s does not match its recorded digest", artifact.Path)
		}
	}
	return nil
}

func (reuse *ciResultReuse) artifactPath(relative string) (string, error) {
	cleaned := filepath.Clean(filepath.FromSlash(strings.TrimSpace(relative)))
	if cleaned == "." || cleaned == ".." || filepath.IsAbs(cleaned) || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid recorded artifact path %q", relative)
	}
	return filepath.Join(reuse.outputDirectory, cleaned), nil
}

// record captures a task that actually executed and passed. Artifact bytes are
// read back from the workspace and re-hashed here, so a record can only name
// content that exists and matches what the report claims.
func (reuse *ciResultReuse) record(task *CIReportTask) (*ciResultRecord, map[string][]byte, error) {
	blobs := map[string][]byte{}
	artifacts := make([]CIReportArtifact, 0, len(task.Artifacts))
	for _, artifact := range task.Artifacts {
		path, err := reuse.artifactPath(artifact.Path)
		if err != nil {
			return nil, nil, err
		}
		payload, err := os.ReadFile(path)
		if err != nil {
			return nil, nil, fmt.Errorf("read artifact %s: %w", artifact.Path, err)
		}
		if artifactDigest(payload) != artifact.SHA256 {
			return nil, nil, fmt.Errorf("artifact %s changed after it was reported", artifact.Path)
		}
		blobs[artifact.SHA256] = payload
		artifacts = append(artifacts, artifact)
	}
	record := &ciResultRecord{
		Schema:         ciResultRecordSchema,
		IdentitySchema: task.Cache.SchemaVersion,
		Identity:       task.Cache.Key,
		Task:           task.ID,
		Phase:          task.Phase,
		Suite:          task.Suite,
		Service:        task.Service,
		Outcome:        ciResultOutcomePass,
		Reference:      reuse.reference,
		Run:            reuse.run,
		Revision:       reuse.revision,
		Environment:    reuse.environment,
		RecordedAt:     formatReportTime(reuse.now()),
		Evidence: ciResultEvidence{
			Audit:     task.Audit,
			Drift:     task.Drift,
			Integrity: task.Integrity,
			Artifacts: artifacts,
		},
	}
	signature, err := record.sign(reuse.key)
	if err != nil {
		return nil, nil, err
	}
	record.Signature = signature
	return record, blobs, nil
}

func (reuse *ciResultReuse) publish(task *CIReportTask) error {
	record, blobs, err := reuse.record(task)
	if err != nil {
		return err
	}
	return reuse.store.publish(record, blobs)
}
