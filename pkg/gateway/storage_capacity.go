package gateway

// ARCHITECTURE: Storage capacity is observed by Codefly because the Gateway is
// the execution authority that owns the filesystem root. Mind supplies named
// operation requirements and receives only typed capacity evidence; host paths
// and filesystem syscalls never cross into the brain.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode"

	"github.com/codefly-dev/core/failures"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	gatewayv1 "github.com/codefly-dev/core/generated/go/mind/gateway/v1"
	"github.com/codefly-dev/core/resources"
)

const maxStorageCapacityRequirements = 64

var errStorageCapacityRequirementsOverflow = errors.New("storage capacity requirements exceed uint64")

// EvaluateStorageCapacity measures the filesystem containing the Gateway root
// and evaluates the caller's typed byte requirements. The result is an
// observation, not a reservation; callers re-evaluate before later expensive
// phases when the first observation may be stale.
func (s *Server) EvaluateStorageCapacity(
	ctx context.Context,
	req *gatewayv1.EvaluateStorageCapacityRequest,
) (*gatewayv1.EvaluateStorageCapacityResponse, error) {
	const operation = "gateway.evaluate-storage-capacity"
	if err := ctx.Err(); err != nil {
		return &gatewayv1.EvaluateStorageCapacityResponse{Failure: failures.FromError(operation, err)}, nil
	}
	requirements, err := canonicalStorageRequirements(req)
	if err != nil {
		return &gatewayv1.EvaluateStorageCapacityResponse{
			Failure: failures.New(basev0.FailureCode_FAILURE_CODE_INVALID_ARGUMENT, operation, err.Error()),
		}, nil
	}
	admissions, err := s.evaluateStorageRequirements(requirements)
	if err != nil {
		code := basev0.FailureCode_FAILURE_CODE_IO_FAILED
		if errors.Is(err, errStorageCapacityRequirementsOverflow) {
			code = basev0.FailureCode_FAILURE_CODE_INVALID_ARGUMENT
		}
		return &gatewayv1.EvaluateStorageCapacityResponse{
			Failure: failures.New(code, operation, "evaluate Codefly storage capacity: "+err.Error()),
		}, nil
	}
	response := &gatewayv1.EvaluateStorageCapacityResponse{Admissions: admissions}
	for _, admission := range admissions {
		if admission.GetRequiredBytes() <= admission.GetAvailableBytes() {
			admission.Admitted = true
			admission.ProjectedAvailableBytes = admission.GetAvailableBytes() - admission.GetRequiredBytes()
			continue
		}
		admission.ShortfallBytes = admission.GetRequiredBytes() - admission.GetAvailableBytes()
		if response.Failure == nil {
			response.Failure = failures.New(
				basev0.FailureCode_FAILURE_CODE_RESOURCE_EXHAUSTED,
				operation,
				fmt.Sprintf("storage authority %s is short by %d bytes (%d required, %d available)", admission.GetAuthorityId(), admission.GetShortfallBytes(), admission.GetRequiredBytes(), admission.GetAvailableBytes()),
			)
		}
	}
	return response, nil
}

func canonicalStorageRequirements(req *gatewayv1.EvaluateStorageCapacityRequest) ([]*basev0.StorageCapacityRequirement, error) {
	if req == nil {
		return nil, fmt.Errorf("storage capacity request is required")
	}
	if len(req.GetRequirements()) == 0 {
		return nil, fmt.Errorf("at least one storage capacity requirement is required")
	}
	if len(req.GetRequirements()) > maxStorageCapacityRequirements {
		return nil, fmt.Errorf("storage capacity requirement count %d exceeds %d", len(req.GetRequirements()), maxStorageCapacityRequirements)
	}
	seen := make(map[string]struct{}, len(req.GetRequirements()))
	canonical := make([]*basev0.StorageCapacityRequirement, 0, len(req.GetRequirements()))
	for index, requirement := range req.GetRequirements() {
		if requirement == nil {
			return nil, fmt.Errorf("storage capacity requirement %d is nil", index)
		}
		component := strings.TrimSpace(requirement.GetComponent())
		if !validStorageRequirementComponent(component) || component != requirement.GetComponent() {
			return nil, fmt.Errorf("storage capacity requirement %d component %q is not a stable identifier", index, requirement.GetComponent())
		}
		if _, exists := seen[component]; exists {
			return nil, fmt.Errorf("storage capacity requirement component %q is duplicated", component)
		}
		if requirement.GetBytes() == 0 {
			return nil, fmt.Errorf("storage capacity requirement %q must be non-zero", component)
		}
		switch requirement.GetAuthorityKind() {
		case basev0.StorageAuthorityKind_STORAGE_AUTHORITY_KIND_GATEWAY_ROOT,
			basev0.StorageAuthorityKind_STORAGE_AUTHORITY_KIND_SERVICE_STATE:
		default:
			return nil, fmt.Errorf("storage capacity requirement %q has unsupported authority kind %s", component, requirement.GetAuthorityKind())
		}
		seen[component] = struct{}{}
		canonical = append(canonical, &basev0.StorageCapacityRequirement{
			Component: component, Bytes: requirement.GetBytes(), AuthorityKind: requirement.GetAuthorityKind(),
		})
	}
	return canonical, nil
}

func (s *Server) evaluateStorageRequirements(requirements []*basev0.StorageCapacityRequirement) ([]*basev0.StorageCapacityAdmission, error) {
	admissions := make([]*basev0.StorageCapacityAdmission, 0, 2)
	byAuthority := make(map[string]*basev0.StorageCapacityAdmission, 2)
	for _, requirement := range requirements {
		root, err := s.storageAuthorityRoot(requirement.GetAuthorityKind())
		if err != nil {
			return nil, err
		}
		filesystem, err := resources.InspectStorageFilesystem(root)
		if err != nil {
			return nil, fmt.Errorf("inspect %s: %w", requirement.GetAuthorityKind(), err)
		}
		admission := byAuthority[filesystem.AuthorityID]
		if admission == nil {
			admission = &basev0.StorageCapacityAdmission{
				AuthorityId: filesystem.AuthorityID, TotalBytes: filesystem.TotalBytes, AvailableBytes: filesystem.AvailableBytes,
			}
			byAuthority[filesystem.AuthorityID] = admission
			admissions = append(admissions, admission)
		} else if filesystem.AvailableBytes < admission.AvailableBytes {
			// Measurements for two logical roots on one live volume can differ by
			// concurrent writes. The conservative reading governs admission.
			admission.AvailableBytes = filesystem.AvailableBytes
		}
		if !containsStorageAuthorityKind(admission.AuthorityKinds, requirement.GetAuthorityKind()) {
			admission.AuthorityKinds = append(admission.AuthorityKinds, requirement.GetAuthorityKind())
		}
		if math.MaxUint64-admission.RequiredBytes < requirement.GetBytes() {
			return nil, fmt.Errorf("%w for authority %s", errStorageCapacityRequirementsOverflow, filesystem.AuthorityID)
		}
		admission.RequiredBytes += requirement.GetBytes()
		admission.Requirements = append(admission.Requirements, requirement)
	}
	return admissions, nil
}

func (s *Server) storageAuthorityRoot(kind basev0.StorageAuthorityKind) (string, error) {
	switch kind {
	case basev0.StorageAuthorityKind_STORAGE_AUTHORITY_KIND_GATEWAY_ROOT:
		return s.cfg.WorkDir, nil
	case basev0.StorageAuthorityKind_STORAGE_AUTHORITY_KIND_SERVICE_STATE:
		return resources.CodeflyHomeDir(), nil
	default:
		return "", fmt.Errorf("unsupported storage authority kind %s", kind)
	}
}

func containsStorageAuthorityKind(values []basev0.StorageAuthorityKind, wanted basev0.StorageAuthorityKind) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func validStorageRequirementComponent(component string) bool {
	if component == "" || len(component) > 128 {
		return false
	}
	for _, character := range component {
		if unicode.IsLetter(character) || unicode.IsDigit(character) {
			continue
		}
		switch character {
		case '.', '-', '_', '/', ':':
			continue
		default:
			return false
		}
	}
	return true
}
