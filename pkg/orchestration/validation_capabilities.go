package orchestration

import agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"

type ValidationOperation string

const (
	ValidationLint          ValidationOperation = "lint"
	ValidationCompile       ValidationOperation = "compile"
	ValidationTest          ValidationOperation = "test"
	ValidationAudit         ValidationOperation = "audit"
	ValidationArtifactBuild ValidationOperation = "artifact_build"
	ValidationSBOM          ValidationOperation = "sbom"
	ValidationSync          ValidationOperation = "sync"
)

// ValidationOperationSupport returns whether an agent has an authoritative
// validation contract and, when it does, whether the named operation is
// supported. A missing contract remains the explicit legacy signal.
func ValidationOperationSupport(info *agentv0.AgentInformation, operation ValidationOperation) (advertised, supported bool) {
	if info == nil || info.GetValidation() == nil {
		return false, false
	}
	validation := info.GetValidation()
	switch operation {
	case ValidationLint:
		return true, validation.GetLint().GetSupported()
	case ValidationCompile:
		return true, validation.GetCompile().GetSupported()
	case ValidationTest:
		return true, validation.GetTest().GetSupported()
	case ValidationAudit:
		return true, validation.GetAudit().GetSupported()
	case ValidationArtifactBuild:
		return true, validation.GetArtifactBuild().GetSupported()
	case ValidationSBOM:
		return true, validation.GetSbom().GetSupported()
	case ValidationSync:
		return true, validation.GetSync().GetSupported()
	default:
		return true, false
	}
}

// validationSupport distinguishes a legacy agent (no validation contract) from
// an authoritative advertisement. Legacy agents retain compatibility probing;
// once validation is present, the advertised support bit is binding.
func validationSupport(info *agentv0.AgentInformation, action ActionType) (advertised, supported bool) {
	switch action {
	case RuntimeLint:
		return ValidationOperationSupport(info, ValidationLint)
	case RuntimeBuild:
		return ValidationOperationSupport(info, ValidationCompile)
	case RuntimeTest:
		return ValidationOperationSupport(info, ValidationTest)
	default:
		return false, false
	}
}
