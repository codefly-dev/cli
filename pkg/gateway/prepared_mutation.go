package gateway

// ARCHITECTURE: prepared mutations are Codefly's half of SaaS write
// admission. Codefly sees project bytes, resolves the exact edit, and seals
// its result. The coordinator sees only the digest and resource identities.
// Apply rechecks the project precondition and an Ed25519-signed permit before
// writing, so Mind cannot turn a planning approval into an arbitrary edit.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/codefly-dev/core/failures"
	codev0 "github.com/codefly-dev/core/generated/go/codefly/services/code/v0"
	gatewayv1 "github.com/codefly-dev/core/generated/go/mind/gateway/v1"
	"github.com/codefly-dev/core/policy"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	preparedMutationSchemaVersion = 1
	preparedMutationLifetime      = 2 * time.Minute
	maxPreparedMutationCount      = 512
	maxPreparedMutationBytes      = 8 << 20
	maxPreparedMutationTotalBytes = 256 << 20
	mutationPermitAction          = "workspace.mutation.apply"
	mutationPermitCaveat          = "mutation_binding"
)

type mutationAuthorityBinding struct {
	authorityID string
	workspaceID string
	publicKey   ed25519.PublicKey
	replay      *policy.ReplayTracker
}

// storedPreparedMutation keeps project bytes inside Codefly. Only its cloned,
// source-free PreparedMutation header crosses the Gateway RPC boundary.
type storedPreparedMutation struct {
	prepared    *gatewayv1.PreparedMutation
	afterByPath map[string][]byte
	expiresAt   time.Time
	byteCount   int
}

// mutationPermitBinding is the coordinator-owned caveat carried by Codefly's
// existing v2 Ed25519 scoped-authorization format.
type mutationPermitBinding struct {
	AuthorityID      string                `json:"authority_id"`
	WorkspaceID      string                `json:"workspace_id"`
	Service          string                `json:"service"`
	TenantID         string                `json:"tenant_id"`
	PlanID           string                `json:"plan_id"`
	PlanRevision     uint64                `json:"plan_revision"`
	PlanContentHash  string                `json:"plan_content_hash"`
	LeaseSetID       string                `json:"lease_set_id"`
	OwnerAttemptID   string                `json:"owner_attempt_id"`
	WorkspaceVersion string                `json:"workspace_version"`
	Fences           []mutationPermitFence `json:"fences"`
}

type mutationPermitFence struct {
	Kind       string `json:"kind"`
	Path       string `json:"path"`
	SymbolID   string `json:"symbol_id,omitempty"`
	FenceToken uint64 `json:"fence_token"`
}

// ConfigureMutationAuthority pins one coordinator key and workspace identity.
// Repeating the byte-identical binding is idempotent; changing it requires a
// new gateway process so an admitted workspace cannot be silently retargeted.
func (s *Server) ConfigureMutationAuthority(_ context.Context, req *gatewayv1.ConfigureMutationAuthorityRequest) (*gatewayv1.ConfigureMutationAuthorityResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "mutation authority request is required")
	}
	authorityID := strings.TrimSpace(req.GetAuthorityId())
	workspaceID := strings.TrimSpace(req.GetWorkspaceId())
	if authorityID == "" || authorityID != req.GetAuthorityId() || workspaceID == "" || workspaceID != req.GetWorkspaceId() {
		return nil, status.Error(codes.InvalidArgument, "authority_id and workspace_id are required and must be canonical")
	}
	if len(req.GetEd25519PublicKey()) != ed25519.PublicKeySize {
		return nil, status.Errorf(codes.InvalidArgument, "Ed25519 public key must be %d bytes", ed25519.PublicKeySize)
	}
	binding := &mutationAuthorityBinding{
		authorityID: authorityID,
		workspaceID: workspaceID,
		publicKey:   append(ed25519.PublicKey(nil), req.GetEd25519PublicKey()...),
		replay:      policy.NewReplayTracker(),
	}
	s.mutationAuthorityMu.Lock()
	defer s.mutationAuthorityMu.Unlock()
	if s.mutationAuthority != nil {
		if s.mutationAuthority.authorityID != binding.authorityID ||
			s.mutationAuthority.workspaceID != binding.workspaceID ||
			!bytes.Equal(s.mutationAuthority.publicKey, binding.publicKey) {
			return nil, status.Error(codes.FailedPrecondition, "mutation authority is already pinned to another binding")
		}
	} else {
		s.mutationAuthority = binding
	}
	return &gatewayv1.ConfigureMutationAuthorityResponse{AuthorityId: authorityID, WorkspaceId: workspaceID}, nil
}

// PrepareMutation resolves one typed text or symbol edit through the real
// language agent in dry-run mode. The resulting bytes and hashes are sealed
// into an immutable protobuf; no write occurs in this RPC.
func (s *Server) PrepareMutation(ctx context.Context, req *gatewayv1.PrepareMutationRequest) (*gatewayv1.PrepareMutationResponse, error) {
	if req == nil {
		return prepareFailure("prepare mutation request is required"), nil
	}
	if err := s.validateService(req.GetService()); err != nil {
		return nil, err
	}
	authority, err := s.currentMutationAuthority()
	if err != nil {
		return prepareFailure(err.Error()), nil
	}
	service := strings.TrimSpace(req.GetService())
	workspaceVersion := strings.TrimSpace(req.GetWorkspaceVersion())
	if service == "" || service != req.GetService() || workspaceVersion == "" || workspaceVersion != req.GetWorkspaceVersion() {
		return prepareFailure("service and workspace_version are required and must be canonical"), nil
	}
	edit, symbolPatch := req.GetApplyEdit(), req.GetSymbolPatch()
	if (edit == nil) == (symbolPatch == nil) {
		return prepareFailure("exactly one apply_edit or symbol_patch mutation is required"), nil
	}
	var path, strategy, symbolID string
	var fixActions []string
	var after []byte
	var previewBeforeHash, previewAfterHash string
	var previewBeforeSize, previewAfterSize uint64
	if edit != nil {
		path, err = cleanGatewayPath(edit.GetFile())
		if err != nil || path == "" {
			if err == nil {
				err = errors.New("edit file is required")
			}
			return prepareFailure(err.Error()), nil
		}
		if edit.GetFind() == "" {
			return prepareFailure("apply_edit find text is required"), nil
		}
		preview, previewErr := s.ApplyEdit(ctx, &gatewayv1.ApplyEditRequest{
			Service: service, File: path, Find: edit.GetFind(), Replace: edit.GetReplace(),
			FixMode: edit.GetFixMode(), DryRun: true,
		})
		if previewErr != nil {
			return prepareFailure(previewErr.Error()), nil
		}
		if preview == nil || !preview.GetSuccess() {
			return prepareFailure(preview.GetError()), nil
		}
		if preview.GetWrote() || !preview.GetChanged() {
			return prepareFailure("prepared edit must change bytes without writing them"), nil
		}
		after = []byte(preview.GetContent())
		strategy, fixActions = preview.GetStrategy(), append([]string(nil), preview.GetFixActions()...)
		previewBeforeHash, previewAfterHash = preview.GetBeforeSha256(), preview.GetAfterSha256()
		previewBeforeSize, previewAfterSize = preview.GetBeforeSizeBytes(), preview.GetAfterSizeBytes()
	} else {
		path, err = cleanGatewayPath(symbolPatch.GetFile())
		if err != nil || path == "" {
			if err == nil {
				err = errors.New("symbol patch file is required")
			}
			return prepareFailure(err.Error()), nil
		}
		symbolID = strings.TrimSpace(symbolPatch.GetSymbolId())
		qualifiedName := strings.TrimSpace(symbolPatch.GetQualifiedName())
		if symbolID == "" || symbolID != symbolPatch.GetSymbolId() || qualifiedName == "" || qualifiedName != symbolPatch.GetQualifiedName() || !validSHA256(symbolPatch.GetExpectedDeclarationSha256()) {
			return prepareFailure("symbol_patch symbol_id, qualified_name, and expected_declaration_sha256 are required and must be canonical"), nil
		}
		raw, previewErr := s.executeSymbolPatch(ctx, &gatewayv1.ApplySymbolPatchRequest{
			Service: service, File: path, QualifiedName: qualifiedName,
			ExpectedDeclarationSha256: symbolPatch.GetExpectedDeclarationSha256(),
			NewSource:                 symbolPatch.GetNewSource(), FixMode: symbolPatch.GetFixMode(), DryRun: true,
		}, path)
		if previewErr != nil {
			return prepareFailure(previewErr.Error()), nil
		}
		preview := raw.GetApplySymbolPatch()
		if preview == nil || !preview.GetSuccess() {
			projected := gatewaySymbolPatchResponse(raw)
			return &gatewayv1.PrepareMutationResponse{
				Success: false, Error: projected.GetError(), Failure: failures.Clone(projected.GetFailure()),
				SymbolPatchFailureReason: projected.GetFailureReason(),
			}, nil
		}
		if preview.GetWrote() || !preview.GetChanged() {
			return prepareFailure("prepared symbol patch must change bytes without writing them"), nil
		}
		after = []byte(preview.GetContent())
		strategy, fixActions = preview.GetStrategy(), append([]string(nil), preview.GetFixActions()...)
		previewBeforeHash, previewAfterHash = preview.GetBeforeSha256(), preview.GetAfterSha256()
		previewBeforeSize, previewAfterSize = preview.GetBeforeSizeBytes(), preview.GetAfterSizeBytes()
	}
	current, err := s.fileOps().ReadFile(ctx, path)
	if err != nil {
		return prepareFailure(fmt.Sprintf("read prepared target: %v", err)), nil
	}
	beforeHash := contentSHA256(current)
	afterHash := contentSHA256(after)
	if previewBeforeHash != beforeHash || previewAfterHash != afterHash || previewBeforeSize != uint64(len(current)) || previewAfterSize != uint64(len(after)) {
		return prepareFailure("language agent preview identities do not match authoritative project bytes"), nil
	}
	prepared := &gatewayv1.PreparedMutation{
		SchemaVersion:    preparedMutationSchemaVersion,
		PreparationId:    uuid.NewString(),
		AuthorityId:      authority.authorityID,
		WorkspaceId:      authority.workspaceID,
		Service:          service,
		WorkspaceVersion: workspaceVersion,
		Files: []*gatewayv1.PreparedFileMutation{{
			Path: path, Operation: gatewayv1.PreparedFileOperation_PREPARED_FILE_OPERATION_MODIFY,
			BeforeSha256: beforeHash, AfterSha256: afterHash,
			BeforeSizeBytes: uint64(len(current)), AfterSizeBytes: uint64(len(after)),
			Strategy: strategy, FixActions: fixActions, SymbolId: symbolID,
		}},
		PreparedAt: timestamppb.Now(), ExpiresAt: timestamppb.New(time.Now().UTC().Add(preparedMutationLifetime)),
	}
	prepared.MutationDigest, err = computePreparedMutationDigest(prepared)
	if err != nil {
		return prepareFailure(err.Error()), nil
	}
	if err := validatePreparedMutation(prepared); err != nil {
		return prepareFailure(err.Error()), nil
	}
	if err := s.storePreparedMutation(prepared, map[string][]byte{path: after}); err != nil {
		return prepareFailure(err.Error()), nil
	}
	return &gatewayv1.PrepareMutationResponse{Success: true, Prepared: prepared}, nil
}

// ApplyPreparedMutation is the only coordinated project write. It verifies
// the coordinator signature and every binding before taking the local apply
// mutex, then rechecks target hashes and writes the exact prepared bytes.
func (s *Server) ApplyPreparedMutation(ctx context.Context, req *gatewayv1.ApplyPreparedMutationRequest) (*gatewayv1.ApplyPreparedMutationResponse, error) {
	if req == nil {
		return applyPreparedFailure("apply prepared mutation request is required"), nil
	}
	if err := s.validateService(req.GetService()); err != nil {
		return nil, err
	}
	authority, err := s.currentMutationAuthority()
	if err != nil {
		return applyPreparedFailure(err.Error()), nil
	}
	preparationID := strings.TrimSpace(req.GetPreparationId())
	mutationDigest := strings.TrimSpace(req.GetMutationDigest())
	if preparationID == "" || preparationID != req.GetPreparationId() || !validSHA256(mutationDigest) || mutationDigest != req.GetMutationDigest() {
		return applyPreparedFailure("preparation_id and mutation_digest are required and must be canonical"), nil
	}
	prepared, afterByPath, err := s.loadPreparedMutation(preparationID, mutationDigest)
	if err != nil {
		return applyPreparedFailure(err.Error()), nil
	}
	if req.GetService() != prepared.GetService() || prepared.GetAuthorityId() != authority.authorityID || prepared.GetWorkspaceId() != authority.workspaceID {
		return applyPreparedFailure("prepared mutation does not match the pinned authority, workspace, and service"), nil
	}
	authorization, binding, err := verifyMutationPermit(req.GetMutationPermit(), authority.publicKey, prepared)
	if err != nil {
		return applyPreparedFailure(err.Error()), nil
	}
	if err := permitCoversPreparedFiles(binding, prepared.GetFiles()); err != nil {
		return applyPreparedFailure(err.Error()), nil
	}

	s.preparedApplyMu.Lock()
	defer s.preparedApplyMu.Unlock()
	for _, file := range prepared.GetFiles() {
		current, err := s.fileOps().ReadFile(ctx, file.GetPath())
		if err != nil {
			return applyPreparedFailure(fmt.Sprintf("read prepared target %q: %v", file.GetPath(), err)), nil
		}
		if contentSHA256(current) != file.GetBeforeSha256() || uint64(len(current)) != file.GetBeforeSizeBytes() {
			return applyPreparedFailure(fmt.Sprintf("prepared target %q drifted after preparation", file.GetPath())), nil
		}
	}
	// BOUNDARY: consumption happens after every project precondition is known
	// to hold and before the first write. The apply mutex plus MaxUses=1 means
	// concurrent replays have exactly one winner.
	if authority.replay == nil {
		return applyPreparedFailure("mutation permit replay protection is not configured"), nil
	}
	if err := authority.replay.Consume(authorization); err != nil {
		return applyPreparedFailure(fmt.Sprintf("consume coordinator mutation permit: %v", err)), nil
	}
	for _, file := range prepared.GetFiles() {
		after, ok := afterByPath[file.GetPath()]
		if !ok || contentSHA256(after) != file.GetAfterSha256() || uint64(len(after)) != file.GetAfterSizeBytes() {
			return applyPreparedFailure(fmt.Sprintf("prepared bytes for %q are unavailable or corrupted", file.GetPath())), nil
		}
		response, err := s.proxyExecute(ctx, &codev0.CodeRequest{Operation: &codev0.CodeRequest_WriteFile{WriteFile: &codev0.WriteFileRequest{
			Path: file.GetPath(), Content: string(after),
		}}})
		if err != nil || response.GetWriteFile() == nil || !response.GetWriteFile().GetSuccess() {
			if err == nil {
				err = errors.New(codeFailureMessage(response))
			}
			return applyPreparedFailure(fmt.Sprintf("write prepared target %q: %v", file.GetPath(), err)), nil
		}
	}
	applied := make([]*gatewayv1.AppliedFileMutation, 0, len(prepared.GetFiles()))
	for _, file := range prepared.GetFiles() {
		applied = append(applied, &gatewayv1.AppliedFileMutation{
			Path: file.GetPath(), Operation: file.GetOperation(),
			BeforeSha256: file.GetBeforeSha256(), AfterSha256: file.GetAfterSha256(),
			BeforeSizeBytes: file.GetBeforeSizeBytes(), AfterSizeBytes: file.GetAfterSizeBytes(),
		})
	}
	s.deletePreparedMutation(prepared.GetPreparationId(), prepared.GetMutationDigest())
	return &gatewayv1.ApplyPreparedMutationResponse{
		Success: true, PreparationId: prepared.GetPreparationId(),
		MutationDigest: prepared.GetMutationDigest(), Files: applied,
	}, nil
}

func (s *Server) currentMutationAuthority() (*mutationAuthorityBinding, error) {
	s.mutationAuthorityMu.RLock()
	defer s.mutationAuthorityMu.RUnlock()
	if s.mutationAuthority == nil {
		return nil, errors.New("mutation authority is not configured")
	}
	return &mutationAuthorityBinding{
		authorityID: s.mutationAuthority.authorityID,
		workspaceID: s.mutationAuthority.workspaceID,
		publicKey:   append(ed25519.PublicKey(nil), s.mutationAuthority.publicKey...),
		replay:      s.mutationAuthority.replay,
	}, nil
}

func (s *Server) storePreparedMutation(prepared *gatewayv1.PreparedMutation, afterByPath map[string][]byte) error {
	if err := validatePreparedMutation(prepared); err != nil {
		return err
	}
	if len(afterByPath) != len(prepared.GetFiles()) {
		return errors.New("prepared mutation bytes do not match its file identities")
	}
	storedBytes := make(map[string][]byte, len(afterByPath))
	storedByteCount := 0
	for _, file := range prepared.GetFiles() {
		content, ok := afterByPath[file.GetPath()]
		if !ok || contentSHA256(content) != file.GetAfterSha256() || uint64(len(content)) != file.GetAfterSizeBytes() {
			return fmt.Errorf("prepared bytes for %q do not match the after identity", file.GetPath())
		}
		if len(content) > maxPreparedMutationBytes || storedByteCount > maxPreparedMutationBytes-len(content) {
			return fmt.Errorf("prepared mutation exceeds the %d-byte retention limit", maxPreparedMutationBytes)
		}
		storedByteCount += len(content)
		storedBytes[file.GetPath()] = append([]byte(nil), content...)
	}
	clone := proto.Clone(prepared).(*gatewayv1.PreparedMutation)
	expiresAt := clone.GetExpiresAt().AsTime().UTC()
	s.preparedMutationMu.Lock()
	defer s.preparedMutationMu.Unlock()
	s.prunePreparedMutationsLocked(time.Now().UTC())
	if s.preparedMutations == nil {
		s.preparedMutations = make(map[string]*storedPreparedMutation)
	}
	if len(s.preparedMutations) >= maxPreparedMutationCount {
		return fmt.Errorf("prepared mutation capacity of %d outstanding handles is exhausted", maxPreparedMutationCount)
	}
	retainedByteCount := 0
	for _, stored := range s.preparedMutations {
		if stored != nil {
			retainedByteCount += stored.byteCount
		}
	}
	if retainedByteCount > maxPreparedMutationTotalBytes-storedByteCount {
		return fmt.Errorf("prepared mutation byte capacity of %d is exhausted", maxPreparedMutationTotalBytes)
	}
	if existing := s.preparedMutations[clone.GetPreparationId()]; existing != nil {
		return errors.New("prepared mutation identity already exists")
	}
	s.preparedMutations[clone.GetPreparationId()] = &storedPreparedMutation{
		prepared: clone, afterByPath: storedBytes, expiresAt: expiresAt, byteCount: storedByteCount,
	}
	return nil
}

func (s *Server) loadPreparedMutation(preparationID, mutationDigest string) (*gatewayv1.PreparedMutation, map[string][]byte, error) {
	now := time.Now().UTC()
	s.preparedMutationMu.Lock()
	defer s.preparedMutationMu.Unlock()
	s.prunePreparedMutationsLocked(now)
	stored := s.preparedMutations[preparationID]
	if stored == nil {
		return nil, nil, errors.New("prepared mutation is unavailable or expired; prepare it again")
	}
	if stored.prepared.GetMutationDigest() != mutationDigest {
		return nil, nil, errors.New("prepared mutation digest does not match its Codefly handle")
	}
	prepared := proto.Clone(stored.prepared).(*gatewayv1.PreparedMutation)
	afterByPath := make(map[string][]byte, len(stored.afterByPath))
	for path, content := range stored.afterByPath {
		afterByPath[path] = append([]byte(nil), content...)
	}
	return prepared, afterByPath, nil
}

func (s *Server) deletePreparedMutation(preparationID, mutationDigest string) {
	s.preparedMutationMu.Lock()
	defer s.preparedMutationMu.Unlock()
	stored := s.preparedMutations[preparationID]
	if stored != nil && stored.prepared.GetMutationDigest() == mutationDigest {
		delete(s.preparedMutations, preparationID)
	}
}

func (s *Server) prunePreparedMutationsLocked(now time.Time) {
	for preparationID, stored := range s.preparedMutations {
		if stored == nil || !now.Before(stored.expiresAt) {
			delete(s.preparedMutations, preparationID)
		}
	}
}

func validatePreparedMutation(prepared *gatewayv1.PreparedMutation) error {
	if prepared == nil || prepared.GetSchemaVersion() != preparedMutationSchemaVersion {
		return errors.New("prepared mutation schema is missing or unsupported")
	}
	if _, err := uuid.Parse(prepared.GetPreparationId()); err != nil {
		return fmt.Errorf("prepared mutation has invalid preparation_id: %w", err)
	}
	for name, value := range map[string]string{
		"authority_id": prepared.GetAuthorityId(), "workspace_id": prepared.GetWorkspaceId(),
		"service": prepared.GetService(), "workspace_version": prepared.GetWorkspaceVersion(),
	} {
		if strings.TrimSpace(value) == "" || value != strings.TrimSpace(value) {
			return fmt.Errorf("prepared mutation %s is required and must be canonical", name)
		}
	}
	if prepared.GetPreparedAt() == nil {
		return errors.New("prepared mutation time is required")
	}
	if err := prepared.GetPreparedAt().CheckValid(); err != nil {
		return fmt.Errorf("prepared mutation time is invalid: %w", err)
	}
	if prepared.GetExpiresAt() == nil {
		return errors.New("prepared mutation expiry is required")
	}
	if err := prepared.GetExpiresAt().CheckValid(); err != nil {
		return fmt.Errorf("prepared mutation expiry is invalid: %w", err)
	}
	if !prepared.GetExpiresAt().AsTime().After(prepared.GetPreparedAt().AsTime()) {
		return errors.New("prepared mutation expiry must follow preparation")
	}
	if len(prepared.GetFiles()) != 1 {
		return errors.New("the first prepared-mutation slice requires exactly one file")
	}
	file := prepared.GetFiles()[0]
	if file == nil || file.GetOperation() != gatewayv1.PreparedFileOperation_PREPARED_FILE_OPERATION_MODIFY {
		return errors.New("prepared mutation requires one modify operation")
	}
	path, err := cleanGatewayPath(file.GetPath())
	if err != nil || path == "" || path != file.GetPath() {
		return errors.New("prepared mutation path is not canonical")
	}
	if !validSHA256(file.GetBeforeSha256()) || !validSHA256(file.GetAfterSha256()) || file.GetBeforeSha256() == file.GetAfterSha256() {
		return errors.New("prepared mutation requires distinct lowercase SHA-256 before/after hashes")
	}
	if file.GetSymbolId() != strings.TrimSpace(file.GetSymbolId()) {
		return errors.New("prepared mutation symbol_id must be canonical when present")
	}
	digest, err := computePreparedMutationDigest(prepared)
	if err != nil {
		return err
	}
	if !validSHA256(prepared.GetMutationDigest()) || prepared.GetMutationDigest() != digest {
		return errors.New("prepared mutation digest is missing or mismatched")
	}
	return nil
}

func computePreparedMutationDigest(prepared *gatewayv1.PreparedMutation) (string, error) {
	if prepared == nil {
		return "", errors.New("prepared mutation is required")
	}
	clone, ok := proto.Clone(prepared).(*gatewayv1.PreparedMutation)
	if !ok || clone == nil {
		return "", errors.New("clone prepared mutation")
	}
	clone.MutationDigest = ""
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(clone)
	if err != nil {
		return "", fmt.Errorf("marshal prepared mutation: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func verifyMutationPermit(raw string, publicKey ed25519.PublicKey, prepared *gatewayv1.PreparedMutation) (*policy.ScopedAuthorization, *mutationPermitBinding, error) {
	if strings.TrimSpace(raw) == "" || raw != strings.TrimSpace(raw) {
		return nil, nil, errors.New("coordinator mutation permit is required and must be canonical")
	}
	if prepared == nil {
		return nil, nil, errors.New("prepared mutation is required for permit verification")
	}
	binding := &mutationPermitBinding{}
	verifyBinding := func(value any) error {
		encoded, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("encode mutation binding: %w", err)
		}
		decoder := json.NewDecoder(bytes.NewReader(encoded))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(binding); err != nil {
			return fmt.Errorf("decode mutation binding: %w", err)
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			return errors.New("mutation binding must contain exactly one JSON value")
		}
		return validateMutationPermitBinding(binding, prepared)
	}
	authorization, err := policy.VerifyEd25519(raw, policy.VerifyExpectations{
		Action:   mutationPermitAction,
		Resource: prepared.GetMutationDigest(),
		Audience: mutationPermitAudience(prepared.GetWorkspaceId()),
		CaveatVerifiers: map[string]policy.CaveatVerifier{
			mutationPermitCaveat: verifyBinding,
		},
		RequiredCaveats: []string{mutationPermitCaveat},
	}, publicKey)
	if err != nil {
		return nil, nil, fmt.Errorf("verify coordinator mutation permit: %w", err)
	}
	// Scoped authorizations allow clock skew for cross-host tool calls. Mutation
	// permits are intentionally shorter-lived and Codefly is the final write
	// authority, so this boundary also enforces the literal expiry.
	if !time.Now().UTC().Before(time.Unix(authorization.ExpiresAtUnix, 0)) {
		return nil, nil, errors.New("coordinator mutation permit is expired")
	}
	if authorization.MaxUses != 1 {
		return nil, nil, errors.New("mutation permit must be single-use")
	}
	if authorization.PrincipalID != binding.AuthorityID || authorization.PrincipalKind != policy.KindService ||
		authorization.PrincipalOrgID != binding.TenantID {
		return nil, nil, errors.New("mutation permit signer identity does not match its coordination binding")
	}
	return authorization, binding, nil
}

func mutationPermitAudience(workspaceID string) string {
	return "codefly-gateway:" + workspaceID
}

func validateMutationPermitBinding(binding *mutationPermitBinding, prepared *gatewayv1.PreparedMutation) error {
	if binding == nil {
		return errors.New("mutation permit binding is required")
	}
	for name, value := range map[string]string{
		"authority_id": binding.AuthorityID, "workspace_id": binding.WorkspaceID,
		"service": binding.Service, "tenant_id": binding.TenantID, "plan_id": binding.PlanID,
		"lease_set_id": binding.LeaseSetID, "owner_attempt_id": binding.OwnerAttemptID,
		"workspace_version": binding.WorkspaceVersion,
	} {
		if strings.TrimSpace(value) == "" || value != strings.TrimSpace(value) {
			return fmt.Errorf("mutation permit %s is required and must be canonical", name)
		}
	}
	if _, err := uuid.Parse(binding.TenantID); err != nil {
		return fmt.Errorf("mutation permit tenant_id is invalid: %w", err)
	}
	if _, err := uuid.Parse(binding.LeaseSetID); err != nil {
		return fmt.Errorf("mutation permit lease_set_id is invalid: %w", err)
	}
	if binding.PlanRevision == 0 || !validSHA256(binding.PlanContentHash) {
		return errors.New("mutation permit plan revision and content hash are required")
	}
	if binding.AuthorityID != prepared.GetAuthorityId() || binding.WorkspaceID != prepared.GetWorkspaceId() ||
		binding.Service != prepared.GetService() || binding.WorkspaceVersion != prepared.GetWorkspaceVersion() {
		return errors.New("mutation permit does not authorize this prepared workspace mutation")
	}
	if len(binding.Fences) == 0 {
		return errors.New("mutation permit must carry at least one fence")
	}
	seen := make(map[string]struct{}, len(binding.Fences))
	for _, fence := range binding.Fences {
		path, err := cleanGatewayPath(fence.Path)
		if err != nil || path == "" || path != fence.Path || fence.FenceToken == 0 {
			return errors.New("mutation permit contains a non-canonical fence")
		}
		switch fence.Kind {
		case "file":
			if fence.SymbolID != "" {
				return errors.New("file mutation fence must not carry symbol_id")
			}
		case "symbol":
			if strings.TrimSpace(fence.SymbolID) == "" || fence.SymbolID != strings.TrimSpace(fence.SymbolID) {
				return errors.New("symbol mutation fence requires canonical symbol_id")
			}
		default:
			return fmt.Errorf("mutation permit has unsupported fence kind %q", fence.Kind)
		}
		key := fmt.Sprintf("%s\x00%s\x00%s", fence.Kind, fence.Path, fence.SymbolID)
		if _, duplicate := seen[key]; duplicate {
			return errors.New("mutation permit contains a duplicate fence")
		}
		seen[key] = struct{}{}
	}
	return nil
}

func permitCoversPreparedFiles(binding *mutationPermitBinding, files []*gatewayv1.PreparedFileMutation) error {
	if binding == nil {
		return errors.New("mutation permit binding is required")
	}
	for _, file := range files {
		covered := false
		for _, fence := range binding.Fences {
			fileFence := file.GetSymbolId() == "" && fence.Kind == "file" && fence.SymbolID == ""
			symbolFence := file.GetSymbolId() != "" && fence.Kind == "symbol" && fence.SymbolID == file.GetSymbolId()
			if fence.Path == file.GetPath() && fence.FenceToken > 0 && (fileFence || symbolFence) {
				covered = true
				break
			}
		}
		if !covered {
			return fmt.Errorf("mutation permit has no exact fence for prepared target %q symbol %q", file.GetPath(), file.GetSymbolId())
		}
	}
	return nil
}

func prepareFailure(message string) *gatewayv1.PrepareMutationResponse {
	if strings.TrimSpace(message) == "" {
		message = "prepare mutation failed"
	}
	return &gatewayv1.PrepareMutationResponse{Success: false, Error: message}
}

func applyPreparedFailure(message string) *gatewayv1.ApplyPreparedMutationResponse {
	if strings.TrimSpace(message) == "" {
		message = "apply prepared mutation failed"
	}
	return &gatewayv1.ApplyPreparedMutationResponse{Success: false, Error: message}
}

func contentSHA256(content []byte) string {
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}
