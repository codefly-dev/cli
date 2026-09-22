package environments

import "testing"

func TestEnvironmentResourceQuotaValidate(t *testing.T) {
	if err := (*EnvironmentResourceQuota)(nil).Validate(); err != nil {
		t.Fatalf("nil quota is a valid not-declared state: %v", err)
	}
	if err := (&EnvironmentResourceQuota{}).Validate(); err == nil {
		t.Fatal("expected an empty quota to fail validation")
	}
	if err := (&EnvironmentResourceQuota{
		DefaultContainer: &EnvironmentContainerResources{},
	}).Validate(); err == nil {
		t.Fatal("expected a quota whose only block is an empty default-container to fail validation")
	}
	valid := []*EnvironmentResourceQuota{
		{Requests: &EnvironmentResourceList{CPU: "4"}},
		{Limits: &EnvironmentResourceList{Memory: "16Gi"}},
		{Pods: "50"},
		{DefaultContainer: &EnvironmentContainerResources{Requests: &EnvironmentResourceList{CPU: "100m"}}},
	}
	for _, quota := range valid {
		if err := quota.Validate(); err != nil {
			t.Fatalf("valid quota %+v rejected: %v", quota, err)
		}
	}
}
