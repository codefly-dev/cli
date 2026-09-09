package deployments

import (
	"fmt"
	"strings"
	"time"
)

// CompletionStage names how far a deployment actually got. The stages are
// totally ordered and cumulative — reaching one means every earlier one holds:
//
//	rendered     manifests were produced from the deployment tree; no cluster
//	             was contacted.
//	applied      kubectl apply accepted every manifest against the verified
//	             target. Nothing has been observed running.
//	bootstrapped every Job the applied manifests own ran to completion.
//	healthy      every workload the applied manifests own finished its rollout
//	             with its replicas ready.
type CompletionStage string

const (
	StageRendered     CompletionStage = "rendered"
	StageApplied      CompletionStage = "applied"
	StageBootstrapped CompletionStage = "bootstrapped"
	StageHealthy      CompletionStage = "healthy"
)

var completionStages = []CompletionStage{StageRendered, StageApplied, StageBootstrapped, StageHealthy}

// DefaultCompletionTimeout bounds observation for a caller that asks for a
// stage beyond applied without naming its own budget.
const DefaultCompletionTimeout = 10 * time.Minute

// CompletionStageNames lists the accepted stages, for flag help and errors.
func CompletionStageNames() []string {
	names := make([]string, 0, len(completionStages))
	for _, stage := range completionStages {
		names = append(names, string(stage))
	}
	return names
}

func ParseCompletionStage(value string) (CompletionStage, error) {
	candidate := CompletionStage(strings.ToLower(strings.TrimSpace(value)))
	for _, stage := range completionStages {
		if candidate == stage {
			return stage, nil
		}
	}
	return "", fmt.Errorf("unknown completion stage %q; expected one of %s", value, strings.Join(CompletionStageNames(), ", "))
}

func (stage CompletionStage) rank() int {
	for index, candidate := range completionStages {
		if candidate == stage {
			return index
		}
	}
	return -1
}

// AtLeast reports whether stage is other or beyond it. An unrecognized stage is
// below every named stage.
func (stage CompletionStage) AtLeast(other CompletionStage) bool {
	return stage.rank() >= other.rank() && stage.rank() >= 0
}

// CompletionCondition is the stage a caller requires before a deployment counts
// as successful, and the budget observation has to establish it.
type CompletionCondition struct {
	Stage   CompletionStage
	Timeout time.Duration
}

// DefaultDeployCompletion is what every direct-apply caller has always
// established: kubectl apply returning success. It stays the default so an
// existing deploy keeps its meaning instead of silently claiming health.
func DefaultDeployCompletion() CompletionCondition {
	return CompletionCondition{Stage: StageApplied, Timeout: DefaultCompletionTimeout}
}

// weakestStage is the stage the whole deployment established: a deployment is
// only as complete as its least complete tree.
func weakestStage(trees []RenderedTreeEvidence) CompletionStage {
	if len(trees) == 0 {
		return ""
	}
	weakest := trees[0].Stage
	for index := range trees[1:] {
		if stage := trees[index+1].Stage; !stage.AtLeast(weakest) {
			weakest = stage
		}
	}
	return weakest
}
