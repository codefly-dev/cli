package orchestration

import (
	"testing"

	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
)

func TestValidationSupportDistinguishesLegacyUnsupportedAndSupported(t *testing.T) {
	tests := []struct {
		name       string
		info       *agentv0.AgentInformation
		action     ActionType
		advertised bool
		supported  bool
	}{
		{name: "legacy", info: &agentv0.AgentInformation{}, action: RuntimeLint},
		{
			name:       "explicitly unsupported",
			info:       &agentv0.AgentInformation{Validation: &agentv0.ValidationCapabilities{}},
			action:     RuntimeLint,
			advertised: true,
		},
		{
			name: "lint supported",
			info: &agentv0.AgentInformation{Validation: &agentv0.ValidationCapabilities{
				Lint: &agentv0.ValidationOperationCapability{Supported: true},
			}},
			action:     RuntimeLint,
			advertised: true,
			supported:  true,
		},
		{
			name: "compile supported",
			info: &agentv0.AgentInformation{Validation: &agentv0.ValidationCapabilities{
				Compile: &agentv0.ValidationOperationCapability{Supported: true},
			}},
			action:     RuntimeBuild,
			advertised: true,
			supported:  true,
		},
		{
			name: "test supported",
			info: &agentv0.AgentInformation{Validation: &agentv0.ValidationCapabilities{
				Test: &agentv0.TestValidationCapability{Supported: true},
			}},
			action:     RuntimeTest,
			advertised: true,
			supported:  true,
		},
		{
			name:       "non-validation action",
			info:       &agentv0.AgentInformation{Validation: &agentv0.ValidationCapabilities{}},
			action:     RuntimeStart,
			advertised: false,
			supported:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			advertised, supported := validationSupport(tt.info, tt.action)
			if advertised != tt.advertised || supported != tt.supported {
				t.Fatalf("validationSupport() = (%v, %v), want (%v, %v)", advertised, supported, tt.advertised, tt.supported)
			}
		})
	}
}

func TestValidationOperationSupportCoversBuilderCapabilities(t *testing.T) {
	info := &agentv0.AgentInformation{Validation: &agentv0.ValidationCapabilities{
		Audit:         &agentv0.ValidationOperationCapability{Supported: true},
		ArtifactBuild: &agentv0.ValidationOperationCapability{Supported: true},
		Sbom:          &agentv0.ValidationOperationCapability{Supported: true},
		Sync:          &agentv0.ValidationOperationCapability{Supported: true},
	}}
	for _, operation := range []ValidationOperation{ValidationAudit, ValidationArtifactBuild, ValidationSBOM, ValidationSync} {
		advertised, supported := ValidationOperationSupport(info, operation)
		if !advertised || !supported {
			t.Fatalf("operation %q support = (%v, %v)", operation, advertised, supported)
		}
	}
	if advertised, supported := ValidationOperationSupport(&agentv0.AgentInformation{}, ValidationAudit); advertised || supported {
		t.Fatalf("legacy operation support = (%v, %v), want compatibility probe", advertised, supported)
	}
}
