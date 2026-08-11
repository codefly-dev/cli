package gateway

import (
	"math"
	"strings"
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	gatewayv1 "github.com/codefly-dev/core/generated/go/mind/gateway/v1"
)

func TestEvaluateStorageCapacityUsesRealGatewayFilesystem(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CODEFLY_HOME", t.TempDir())
	server, err := NewServer(Config{WorkDir: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	response, err := server.EvaluateStorageCapacity(t.Context(), &gatewayv1.EvaluateStorageCapacityRequest{
		Requirements: []*basev0.StorageCapacityRequirement{
			{Component: "repository-snapshot", Bytes: 1, AuthorityKind: basev0.StorageAuthorityKind_STORAGE_AUTHORITY_KIND_GATEWAY_ROOT},
			{Component: "postgres-semantic-publication", Bytes: 2, AuthorityKind: basev0.StorageAuthorityKind_STORAGE_AUTHORITY_KIND_SERVICE_STATE},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.GetFailure() != nil || len(response.GetAdmissions()) != 1 {
		t.Fatalf("admissions = %+v failure = %+v", response.GetAdmissions(), response.GetFailure())
	}
	admission := response.GetAdmissions()[0]
	if !admission.GetAdmitted() || len(admission.GetAuthorityKinds()) != 2 ||
		admission.GetAuthorityKinds()[0] != basev0.StorageAuthorityKind_STORAGE_AUTHORITY_KIND_GATEWAY_ROOT ||
		admission.GetAuthorityKinds()[1] != basev0.StorageAuthorityKind_STORAGE_AUTHORITY_KIND_SERVICE_STATE ||
		!strings.HasPrefix(admission.GetAuthorityId(), "storage/sha256:") || strings.Contains(admission.GetAuthorityId(), root) {
		t.Fatalf("authority = %q kinds = %v", admission.GetAuthorityId(), admission.GetAuthorityKinds())
	}
	if admission.GetTotalBytes() == 0 || admission.GetAvailableBytes() == 0 || admission.GetRequiredBytes() != 3 ||
		admission.GetProjectedAvailableBytes() != admission.GetAvailableBytes()-3 || admission.GetShortfallBytes() != 0 {
		t.Fatalf("capacity evidence = %+v", admission)
	}
}

func TestEvaluateStorageCapacityReturnsTypedResourceExhaustion(t *testing.T) {
	server, err := NewServer(Config{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	response, err := server.EvaluateStorageCapacity(t.Context(), &gatewayv1.EvaluateStorageCapacityRequest{
		Requirements: []*basev0.StorageCapacityRequirement{{
			Component: "semantic-publication", Bytes: math.MaxUint64,
			AuthorityKind: basev0.StorageAuthorityKind_STORAGE_AUTHORITY_KIND_GATEWAY_ROOT,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.GetAdmissions()) != 1 {
		t.Fatalf("admissions = %+v", response.GetAdmissions())
	}
	admission := response.GetAdmissions()[0]
	if admission == nil || admission.GetAdmitted() || admission.GetShortfallBytes() == 0 || admission.GetProjectedAvailableBytes() != 0 {
		t.Fatalf("capacity evidence = %+v", admission)
	}
	if failure := response.GetFailure(); failure == nil || failure.GetCode() != basev0.FailureCode_FAILURE_CODE_RESOURCE_EXHAUSTED {
		t.Fatalf("failure = %+v", failure)
	}
}

func TestEvaluateStorageCapacityRejectsAmbiguousRequirements(t *testing.T) {
	server, err := NewServer(Config{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	response, err := server.EvaluateStorageCapacity(t.Context(), &gatewayv1.EvaluateStorageCapacityRequest{
		Requirements: []*basev0.StorageCapacityRequirement{
			{Component: "snapshot", Bytes: 1, AuthorityKind: basev0.StorageAuthorityKind_STORAGE_AUTHORITY_KIND_GATEWAY_ROOT},
			{Component: "snapshot", Bytes: 2, AuthorityKind: basev0.StorageAuthorityKind_STORAGE_AUTHORITY_KIND_SERVICE_STATE},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.GetAdmissions()) != 0 || response.GetFailure().GetCode() != basev0.FailureCode_FAILURE_CODE_INVALID_ARGUMENT {
		t.Fatalf("response = %+v", response)
	}
}

func TestEvaluateStorageCapacityRejectsPerAuthorityOverflow(t *testing.T) {
	server, err := NewServer(Config{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	response, err := server.EvaluateStorageCapacity(t.Context(), &gatewayv1.EvaluateStorageCapacityRequest{
		Requirements: []*basev0.StorageCapacityRequirement{
			{Component: "semantic-publication", Bytes: math.MaxUint64, AuthorityKind: basev0.StorageAuthorityKind_STORAGE_AUTHORITY_KIND_GATEWAY_ROOT},
			{Component: "repository-snapshot", Bytes: 1, AuthorityKind: basev0.StorageAuthorityKind_STORAGE_AUTHORITY_KIND_GATEWAY_ROOT},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.GetAdmissions()) != 0 || response.GetFailure().GetCode() != basev0.FailureCode_FAILURE_CODE_INVALID_ARGUMENT {
		t.Fatalf("response = %+v", response)
	}
}
