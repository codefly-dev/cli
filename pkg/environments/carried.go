package environments

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/codefly-dev/core/resources"
)

// Carried holds the declarations a module agent owns on an environment block:
// keys the CLI admits, preserves on load and save, and hands verbatim to the
// module bundle generator (encodeTransportNeutralModuleWorkspace), but never
// interprets, never validates the value of, and never sends to a service agent
// (Runtime projects none of them). It is the inline seam core's
// resources.Environment.Extensions already is, held to a closed list of names.
//
// The list is closed because the rest of the environment block is strict: a
// key the CLI does not know is refused by name at workspace load, so a mistyped
// `managed-services` cannot leave a declaration that looks written and does
// nothing. An open map would accept `key-servce` as silently as `key-service`.
// Each name below is a module agent's documented declaration; its value is the
// agent's to validate, and the agent refuses an unsupported one by name.
type Carried map[string]resources.YAMLValue

// CarriedEnvironmentKeys are the module-owned keys an environment may declare.
//
//   - key-service: which key service holds this environment's keys, in the
//     vocabulary of the `key-service` configuration group's
//     KEY_SERVICE_BACKEND. A module agent reads it to decide whether it
//     composes a secrets store in this environment at all.
var CarriedEnvironmentKeys = []string{"key-service"}

// CarriedManagedServiceKeys are the module-owned keys a managed-services entry
// may declare, beside the ones the CLI itself reads.
//
//   - auth-mode: how the workload authenticates to the managed service
//     (`password` through secret-references, or `external-identity` as its own
//     workload identity).
//   - egress-namespace: for an in-cluster managed dependency, the namespace the
//     caller's egress policy selects on.
//   - instance-connection-name: the `project:region:instance` coordinate a
//     Cloud SQL instance is addressed by, which is not a DNS name.
var CarriedManagedServiceKeys = []string{"auth-mode", "egress-namespace", "instance-connection-name"}

// MarshalJSON writes each carried value as the plain JSON value it spells
// (`"key-service": "kms"`), never the YAML node that holds it, so a JSON
// projection of an environment (`codefly environment show --json`) reads like
// the workspace document.
func (carried Carried) MarshalJSON() ([]byte, error) {
	plain := make(map[string]any, len(carried))
	for key := range carried {
		value := carried[key]
		var decoded any
		if err := value.Decode(&decoded); err != nil {
			return nil, fmt.Errorf("carried key %q: %w", key, err)
		}
		plain[key] = decoded
	}
	return json.Marshal(plain)
}

// admit refuses any carried key outside permitted, naming it and the block it
// was written in, so a typo fails the load the way an unknown field does.
func (carried Carried) admit(label string, permitted []string) error {
	var unknown []string
	for key := range carried {
		if !slices.Contains(permitted, key) {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return fmt.Errorf("field %q not found in %s: it is neither a Codefly field nor a module-owned key the CLI carries there (%s)",
		unknown[0], label, strings.Join(permitted, ", "))
}

// admitCarried refuses a key outside the carried lists on the environment or
// on any of its managed services. It runs in Validate rather than in an
// UnmarshalYAML: a custom unmarshaller decodes through a fresh yaml.Node
// decoder, which drops the strict decoder's KnownFields for every nested block.
func (env *Environment) admitCarried() error {
	if err := env.Carried.admit("environment", CarriedEnvironmentKeys); err != nil {
		return err
	}
	keys := make([]string, 0, len(env.ManagedServices))
	for key := range env.ManagedServices {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if err := env.ManagedServices[key].Carried.admit(fmt.Sprintf("managed service %q", key), CarriedManagedServiceKeys); err != nil {
			return err
		}
	}
	return nil
}
