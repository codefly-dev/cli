package deploysecrets

// Rotation: `codefly deploy secrets --rotate`.
//
// A plan keeps every stored value. --rotate is the one exception, and it is
// narrow: it names a key the environment GENERATES (service-secrets.generate),
// mints a new value for it the way the generator would, writes that value to
// every remote key holding it, and rewrites every stored value derived from the
// old one — in the same apply, so a source and what is derived from it never
// disagree. A supplied, configured or propagated-only key is refused: codefly
// does not produce it, so it cannot produce the next one either.
//
// What "derived" means here is what the store can show, not what a name
// suggests. A producer that declares its connection as a template over its own
// secrets (core's ConfigurationValue.template, see gitops/configuration_templates.go)
// has it assembled by External Secrets in the cluster: nothing is stored, so a
// rotated primitive is picked up on the next sync and there is nothing to
// rewrite. A connection an operator stored instead — the one agents export
// without a template, which the plan reports as `require` — embeds the old
// value. Every stored property holding the old value is found by reading the
// store, and one that is a URL whose userinfo password IS the old value is
// recomputed by setting that password and nothing else. Any other embedding is
// refused, naming the property, because codefly cannot recompute it and leaving
// it on the old value is the failure rotation exists to prevent.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/cli/pkg/secretgen"
	"github.com/codefly-dev/core/resources"
)

// RotationKey resolves a --rotate selector to the secret key it names. The
// selector is spelled like the service-secrets.generate entry that declares the
// key: `<module>/<service>/<configuration>:<KEY>` for a service configuration,
// `<configuration>:<KEY>` for a workspace one. A CODEFLY__…_SECRET_CONFIGURATION
// key is taken as written.
func RotationKey(selector string) (string, error) {
	selector = strings.TrimSpace(selector)
	if configurationKey(selector) {
		return selector, nil
	}
	cut := strings.LastIndex(selector, ":")
	if cut <= 0 || cut == len(selector)-1 {
		return "", fmt.Errorf("--rotate %q: want <module>/<service>/<configuration>:<KEY>, <configuration>:<KEY>, or a CODEFLY__…_SECRET_CONFIGURATION__ key", selector)
	}
	parts, key := strings.Split(selector[:cut], "/"), selector[cut+1:]
	if slices.Contains(parts, "") {
		return "", fmt.Errorf("--rotate %q: an empty module, service or configuration", selector)
	}
	switch len(parts) {
	case 1:
		return resources.WorkspaceSecretConfigurationPrefix + "__" + resources.NameToKey(parts[0]) + "__" + resources.NameToKey(key), nil
	case 3:
		return resources.ServiceSecretConfigurationEnvironmentKeyPrefixFromUnique(parts[0]+"/"+parts[1]) +
			"__" + resources.NameToKey(parts[2]) + "__" + resources.NameToKey(key), nil
	default:
		return "", fmt.Errorf("--rotate %q: want <module>/<service>/<configuration>:<KEY> or <configuration>:<KEY>", selector)
	}
}

// ReadsKey reports whether any rendered ExternalSecret reads key.
func ReadsKey(rendered gitops.RenderedEnvironment, key string) bool {
	for _, secret := range rendered.Secrets {
		for _, property := range secret.Properties {
			if slices.Contains(property.Keys, key) {
				return true
			}
		}
	}
	return false
}

// CheckRotationStores refuses a rotation whose configuration is read through
// more than one store. A plan reads and writes one store, so a value of the
// same configuration filed in another — the connection a consumer reads
// through its own store — would be invisible to the rotation and left on the
// old value.
func CheckRotationStores(groups []gitops.RenderedEnvironment, key string) error {
	cut := strings.LastIndex(key, "__")
	if cut < 0 {
		return nil
	}
	configuration := key[:cut+2]
	var stores []string
	for _, group := range groups {
		for _, secret := range group.Secrets {
			for _, property := range secret.Properties {
				for _, read := range property.Keys {
					if strings.HasPrefix(read, configuration) && !slices.Contains(stores, secret.Store.Name) {
						stores = append(stores, secret.Store.Name)
					}
				}
			}
		}
	}
	if len(stores) > 1 {
		return fmt.Errorf("--rotate %s: its configuration is read through %d stores (%s), and a plan rotates within one store; a value derived from it in another would be left on the old one",
			key, len(stores), strings.Join(stores, ", "))
	}
	return nil
}

// derivedRewrite is a stored value recomputed from rotated keys.
type derivedRewrite struct {
	value string
	from  []string
}

// planRotations validates every --rotate key, mints its new value into
// generated, and finds every stored value derived from its old one. It returns
// the rotated keys and the derived rewrites, by location.
func planRotations(in *Inputs, states []*remoteState, generators map[string]environments.EnvironmentSecretGenerator,
	generated map[string]string, aliases map[propertyLocation][]string) (map[string]bool, map[propertyLocation]derivedRewrite, error) {
	rotating := map[string]bool{}
	derived := map[propertyLocation]derivedRewrite{}
	for _, key := range in.Rotate {
		if rotating[key] {
			continue
		}
		generator, declared := generators[key]
		if !declared {
			return nil, nil, refuseRotation(in.Rendered, key)
		}
		if generator.Format == environments.SecretGeneratorFormatEd25519PKCS8 {
			return nil, nil, fmt.Errorf("--rotate %s: it is an ed25519-pkcs8 signing key, whose public half is registered outside this store; rotating it here would break that registration silently", key)
		}
		format, size := generatorFormat(&generator)
		value, err := secretgen.Generate(format, size)
		if err != nil {
			return nil, nil, err
		}
		rotating[key], generated[key] = true, value

		// Every holder rotates together, so every holder must be in scope.
		var old string
		for _, state := range states {
			for _, property := range state.secret.Properties {
				if !slices.Contains(aliases[propertyLocation{state.secret.RemoteKey, property.Property}], key) {
					continue
				}
				if !planning(&state.secret, in.Modules) {
					return nil, nil, fmt.Errorf("--rotate %s: it is also held in %s#%s, outside --module %s; widen the scope so every holder rotates together",
						key, state.secret.RemoteKey, property.Property, strings.Join(in.Modules, ","))
				}
				if stored, present := state.document[property.Property]; present {
					old = stored
				}
			}
		}
		if old == "" {
			// Stored nowhere (or not read): there is nothing to rotate away from,
			// so nothing can be derived from it either.
			continue
		}
		for _, state := range states {
			for _, property := range state.secret.Properties {
				location := propertyLocation{state.secret.RemoteKey, property.Property}
				if slices.Contains(aliases[location], key) {
					continue
				}
				current, present := state.document[property.Property]
				if rewrite, ok := derived[location]; ok {
					current = rewrite.value
				}
				if !present || !strings.Contains(current, old) {
					continue
				}
				where := fmt.Sprintf("%s#%s (%s)", state.secret.RemoteKey, property.Property, strings.Join(property.Keys, ", "))
				rewritten, ok := rewriteURLPassword(current, old, value)
				if !ok {
					return nil, nil, fmt.Errorf("--rotate %s: %s embeds its value other than as a URL's password, which is the one derivation codefly recomputes; rotating would leave it stale — declare the producer's connection as a template, or rotate both by hand",
						key, where)
				}
				if !planning(&state.secret, in.Modules) {
					return nil, nil, fmt.Errorf("--rotate %s: %s is derived from it and lies outside --module %s; widen the scope so it is rewritten in the same apply",
						key, where, strings.Join(in.Modules, ","))
				}
				derived[location] = derivedRewrite{value: rewritten, from: append(derived[location].from, key)}
			}
		}
	}
	return rotating, derived, nil
}

// refuseRotation says why a key no generator declares cannot be rotated.
func refuseRotation(rendered gitops.RenderedEnvironment, key string) error {
	if !ReadsKey(rendered, key) {
		return fmt.Errorf("--rotate %s: no rendered ExternalSecret reads it", key)
	}
	return fmt.Errorf("--rotate %s: service-secrets.generate does not declare it, so codefly does not produce its value — it is supplied (configured or by the operator) and is rotated where it is supplied", key)
}

// rotatedProperty plans one property of a rotated key: the new value replaces a
// stored one, or seeds an absent one exactly as the generator would.
func rotatedProperty(state *remoteState, property gitops.RenderedSecretProperty, key, value string) (PropertyPlan, string, error) {
	plan := PropertyPlan{Property: property.Property, Keys: property.Keys, Action: ActionRotate,
		Source: "--rotate: a new service-secrets.generate value for " + key}
	if !state.known() {
		plan.Source = "--rotate " + key + " if stored; values derived from it are found when the store is read"
		return plan, "", nil
	}
	if _, present := state.document[property.Property]; !present {
		plan.Action, plan.Source = ActionGenerate, "--rotate: not stored here yet, generated for "+key
	}
	return plan, value, nil
}

// rewriteURLPassword sets the userinfo password of a URL whose password is old,
// changing nothing else: a value that does not round-trip through the parser
// byte for byte, or still holds old elsewhere, is not rewritten. Neither the
// value nor a parse error is ever returned to a caller that prints.
func rewriteURLPassword(value, old, replacement string) (string, bool) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.User == nil || parsed.String() != value {
		return "", false
	}
	if password, set := parsed.User.Password(); !set || password != old {
		return "", false
	}
	parsed.User = url.UserPassword(parsed.User.Username(), replacement)
	rewritten := parsed.String()
	if strings.Contains(rewritten, old) {
		return "", false
	}
	return rewritten, true
}

// restore returns a rotation that failed part-way to where it started: every
// key it already wrote gets its previous document back as a new version (a key
// it created gets an empty one), so no source is left disagreeing with what is
// derived from it. The store keeps every version, the rotated ones included.
func (plan *Plan) restore(ctx context.Context, store Store, written []Written, done []pendingWrite, cause error) ([]Written, error) {
	var failed []error
	var stranded []string
	for i := len(done) - 1; i >= 0; i-- {
		write := done[i]
		previous := write.previous
		if previous == nil {
			previous = map[string]string{}
		}
		version, err := store.Write(ctx, write.key, previous, false)
		if err != nil {
			failed, stranded = append(failed, fmt.Errorf("restore %s: %w", write.key, err)), append(stranded, write.key)
			continue
		}
		written = append(written, Written{Key: write.key, Properties: write.changed, Version: version, Restored: true})
	}
	if len(failed) > 0 {
		// A re-run would not converge from here: the rotated value is no longer
		// the old one the derived values embed. The previous versions are kept.
		return written, fmt.Errorf("%w; %s still hold the rotated documents and could not be restored — re-add each one's previous version as its current version before rotating again: %w",
			cause, strings.Join(stranded, ", "), errors.Join(failed...))
	}
	if len(done) == 0 {
		return written, fmt.Errorf("%w; nothing had been written", cause)
	}
	return written, fmt.Errorf("%w; the %d remote keys already written were restored to their previous documents as new versions, so nothing is half-rotated", cause, len(done))
}
