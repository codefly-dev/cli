package runnables

import (
	"errors"
	"fmt"
	"strings"
	"time"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	runnablev0 "github.com/codefly-dev/core/generated/go/codefly/runnable/v0"
	corerunnable "github.com/codefly-dev/core/runnable"
)

// BindingKey is the configuration key an operation is bound under: its module
// and its derived name, upper-cased with every other character an underscore,
// so the SDK's workspace-value lookup finds it. It is also the key an
// environment answers the operation's scope slots under
// (runnable-scope-selections in workspace.codefly.yaml).
//
// It lives here rather than beside one command because two of them need the
// same spelling: the generator writes the value, and a listing says what has
// been answered. A spelling that differed between them would report a grant
// against a key nothing installs.
func BindingKey(module, name string) string {
	key := strings.Builder{}
	for _, r := range strings.ToUpper(module + "__" + name) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			key.WriteRune(r)
			continue
		}
		key.WriteRune('_')
	}
	return key.String()
}

// Spec is the operation's declared policy as core's own OperationSpec, which
// is what core's rules are written against: spelling is how the owner names
// the operation on the wire, and codes is the vocabulary its retryable codes
// are spelled in.
func (o *Operation) Spec(spelling string, codes corerunnable.CodeVocabulary) (*corerunnable.OperationSpec, error) {
	if o == nil {
		return nil, errors.New("the operation declares no execution policy")
	}
	durations := map[string]time.Duration{}
	for field, spelled := range map[string]string{
		"attempt_timeout": o.AttemptTimeout,
		"total_timeout":   o.TotalTimeout,
		"backoff":         o.Backoff,
	} {
		parsed, err := time.ParseDuration(spelled)
		if err != nil {
			return nil, fmt.Errorf("%s %q is not a duration: %w", field, spelled, err)
		}
		durations[field] = parsed
	}
	completion, named := basev0.RunnableExecution_Completion_value[o.Completion]
	if !named || basev0.RunnableExecution_Completion(completion) == basev0.RunnableExecution_COMPLETION_UNKNOWN {
		return nil, fmt.Errorf("completion %q is not a completion mode; %s and %s are the two",
			o.Completion, basev0.RunnableExecution_COMPLETION_CALL, basev0.RunnableExecution_COMPLETION_SUBMIT)
	}
	var recovery basev0.RunnableExecution_Recovery
	if o.Recovery != "" {
		value, known := basev0.RunnableExecution_Recovery_value[o.Recovery]
		if !known || basev0.RunnableExecution_Recovery(value) == basev0.RunnableExecution_RECOVERY_UNKNOWN {
			return nil, fmt.Errorf("recovery %q is not a recovery; %s and %s are the two",
				o.Recovery, basev0.RunnableExecution_RECOVERY_RECOMPUTE, basev0.RunnableExecution_RECOVERY_RECEIPT)
		}
		recovery = basev0.RunnableExecution_Recovery(value)
	}
	return &corerunnable.OperationSpec{
		Method:             spelling,
		Codes:              codes,
		AttemptTimeout:     durations["attempt_timeout"],
		TotalTimeout:       durations["total_timeout"],
		MaxAttempts:        o.MaxAttempts,
		Backoff:            durations["backoff"],
		RetryableCodes:     o.RetryableCodes,
		Audience:           o.Audience,
		InvokeScopes:       specScopes(o.InvokeScopes),
		LookupScopes:       specScopes(o.LookupScopes),
		LookupMethod:       o.LookupMethod,
		MaxInputBytes:      o.MaxInputBytes,
		MaxOutputBytes:     o.MaxOutputBytes,
		Completion:         basev0.RunnableExecution_Completion(completion),
		Tool:               o.Tool,
		RequiredScopeSlots: o.RequiredScopeSlots,
		Recovery:           recovery,
	}, nil
}

func specScopes(declared []Scope) []*basev0.WorkScopeV1 {
	scopes := make([]*basev0.WorkScopeV1, 0, len(declared))
	for _, scope := range declared {
		scopes = append(scopes, &basev0.WorkScopeV1{ResourceKind: scope.ResourceKind, Actions: scope.Actions, ResourceIds: scope.ResourceIDs})
	}
	return scopes
}

// CodeVocabularyFor is the vocabulary an operation's retryable codes are
// spelled in, read off the way its owner names it: a Connect or gRPC procedure
// is a path (`/pkg.Service/Method`), and a REST route is a verb and a path
// (`GET /v1/things`), so the space is the discriminator. Nothing else
// distinguishes them, and a derived operation carries no other field saying
// which it is.
func CodeVocabularyFor(spelling string) corerunnable.CodeVocabulary {
	if strings.Contains(strings.TrimSpace(spelling), " ") {
		return corerunnable.HTTPStatusCodes
	}
	return corerunnable.GRPCStatusNames
}

// Grant is what one environment has answered about one derived operation: the
// configuration key the answer is written under, whether a binding is prepared
// for it, and — when it is not — why. A module version that adds an operation
// adds an unanswered row, which is how "this version offers a tool nobody
// granted" becomes visible without comparing two releases by hand.
type Grant struct {
	// Key is the runnable-bindings key, which is also the
	// runnable-scope-selections key an answer is written under.
	Key string `json:"key"`
	// State is `selected`, `declined`, `needs-scopes` or `invalid`.
	State string `json:"state"`
	// Slots names the owner's required scope slots, when the operation
	// declares any: the questions an answer must resolve.
	Slots []string `json:"slots,omitempty"`
	// Reason is why an `invalid` answer is one, in the words generation
	// refuses it with. Empty for every other state.
	Reason string `json:"reason,omitempty"`
}

// Grant states, which are the whole vocabulary a listing reports.
const (
	// GrantSelected is an operation this environment prepares a binding for:
	// its answer resolves every slot the owner declared, or it declared none.
	GrantSelected = "selected"
	// GrantDeclined is an operation answered with an empty list: installed
	// for no resource, granted no authority, offered as no tool.
	GrantDeclined = "declined"
	// GrantNeedsScopes is an operation whose owner asks for scope slots this
	// environment has not answered at all. Generation refuses it by name
	// rather than preparing a binding with the question still open, so an
	// unanswered slot is work to do, not a quiet omission.
	GrantNeedsScopes = "needs-scopes"
	// GrantInvalid is an answer generation will refuse: a selection that does
	// not resolve the operation's slots, or a decline of an operation that has
	// nothing to decline. Reporting it as granted would send an operator away
	// from the one command that would have told them.
	GrantInvalid = "invalid"
)

// GrantOf is how an environment's answer stands against one derived
// operation, judged by the rules generation judges it by rather than by
// whether an answer is present:
//
//   - an empty list declines, and only an operation that declares a required
//     scope slot has anything to decline (generate's declinableOperation);
//   - otherwise the answer must resolve every slot the owner declared, which
//     is core's own ResolveScopeSlots — it refuses a slot left unselected, a
//     slot named that the operation does not declare, a selection with no
//     invoke scope, an invoke scope missing an action the owner requires, and
//     a missing lookup scope where the slot asks for one;
//   - an operation declaring no slot and answered by nothing is simply
//     prepared.
//
// Anything core refuses is `invalid` carrying core's reason, because a listing
// that reported it as granted would be wrong in exactly the place an operator
// looks to check their work.
func GrantOf(module string, operation *Derived, selections []*runnablev0.ScopeSelection, answered bool) *Grant {
	grant := &Grant{Key: BindingKey(module, operation.Entry.Name), State: GrantSelected, Slots: operation.Slots()}
	if answered && len(selections) == 0 {
		grant.State = GrantDeclined
		if len(grant.Slots) == 0 {
			grant.State, grant.Reason = GrantInvalid, "declines the binding, but the operation declares no required scope slot: there is nothing to decline; remove the entry"
		}
		return grant
	}
	if len(grant.Slots) > 0 && !answered {
		grant.State = GrantNeedsScopes
		return grant
	}
	spec, err := operation.Operation.Spec(operation.Entry.Method, CodeVocabularyFor(operation.Entry.Method))
	if err != nil {
		grant.State, grant.Reason = GrantInvalid, err.Error()
		return grant
	}
	if _, err = spec.ResolveScopeSlots(selections); err != nil {
		grant.State, grant.Reason = GrantInvalid, err.Error()
	}
	return grant
}

// UnansweredKeys is every selection key in answers that no derived operation
// of these modules binds. Generation refuses such a key by name
// (generate's unknownSelectionKey), and a listing that only walked the
// catalogue would never show it: the likeliest mistake with a hand-typed key
// is a typo, which produces an answer to no question and an operation that
// still reports as unanswered. Sorted.
func UnansweredKeys(answers []string, bound map[string]bool) []string {
	unanswered := make([]string, 0)
	for _, key := range answers {
		if !bound[key] {
			unanswered = append(unanswered, key)
		}
	}
	return unanswered
}
