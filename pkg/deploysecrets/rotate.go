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
// value. Every stored property of the store is examined: one that parses as a
// URL whose DECODED userinfo password is the old value is recomputed by
// replacing the password in its userinfo and nothing else. Any other value
// holding the old one, raw or percent-encoded, is refused, naming the property,
// because codefly cannot recompute it and leaving it on the old value is the
// failure rotation exists to prevent.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
	"time"

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
		switch generator.Format {
		case environments.SecretGeneratorFormatEd25519PKCS8:
			return nil, nil, fmt.Errorf("--rotate %s: it is an ed25519-pkcs8 signing key, whose public half is registered outside this store; rotating it here would break that registration silently", key)
		case environments.SecretGeneratorFormatIdentifier:
			return nil, nil, fmt.Errorf("--rotate %s: it is an identifier (a user or database name, say); a new one in the store renames nothing on the server that owns it", key)
		}
		format, size := generator.Encoding()
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
				if !present {
					continue
				}
				rewritten, matched := rewriteURLPassword(current, old, value)
				if !matched && !embeds(current, old) {
					continue
				}
				where := fmt.Sprintf("%s#%s (%s)", state.secret.RemoteKey, property.Property, strings.Join(property.Keys, ", "))
				if rewritten == "" {
					return nil, nil, fmt.Errorf("--rotate %s: %s embeds its value other than as a URL's password codefly can rewrite in place, the one derivation it recomputes; rotating would leave it stale — declare the producer's connection as a template, or rotate both by hand",
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

// embeds reports whether value holds old raw or in any percent-encoding a URL
// would give it.
func embeds(value, old string) bool {
	for _, form := range []string{old, url.QueryEscape(old), url.PathEscape(old), escapedPassword(old)} {
		if strings.Contains(value, form) {
			return true
		}
	}
	return false
}

func escapedPassword(password string) string {
	return strings.TrimPrefix(url.UserPassword("", password).String(), ":")
}

// rewriteURLPassword matches a URL whose decoded userinfo password is old and
// returns it with that password replaced and every other byte as stored: the
// userinfo is spliced, never re-serialized from the parsed URL. matched with an
// empty result is a match codefly cannot rewrite safely — the caller refuses
// it. Neither the value nor a parse error is ever returned to a caller that
// prints.
func rewriteURLPassword(value, old, replacement string) (string, bool) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.User == nil {
		return "", false
	}
	if password, set := parsed.User.Password(); !set || password != old {
		return "", false
	}
	scheme := strings.Index(value, "://")
	if scheme < 0 {
		return "", true
	}
	authority := value[scheme+3:]
	if end := strings.IndexAny(authority, "/?#"); end >= 0 {
		authority = authority[:end]
	}
	at := strings.LastIndex(authority, "@")
	colon := strings.Index(authority[:max(at, 0)], ":")
	if at < 0 || colon < 0 {
		return "", true
	}
	userinfoStart := scheme + 3
	rewritten := value[:userinfoStart+colon+1] + escapedPassword(replacement) + value[userinfoStart+at:]
	check, err := url.Parse(rewritten)
	if err != nil || check.User == nil || check.User.Username() != parsed.User.Username() || check.Host != parsed.Host ||
		check.Path != parsed.Path || check.RawQuery != parsed.RawQuery || check.Fragment != parsed.Fragment || embeds(rewritten, old) {
		return "", true
	}
	if password, _ := check.User.Password(); password != replacement {
		return "", true
	}
	return rewritten, true
}

// restoreTimeout bounds the undo of a failed rotation. It runs on a context of
// its own: the failure that triggered it is often the caller's context ending,
// and an undo that inherits that cancellation undoes nothing.
const restoreTimeout = 2 * time.Minute

// restore returns a rotation that failed part-way to where it started: every
// key it wrote gets its previous document back as a new version (a key it
// created gets an empty one), so no source is left disagreeing with what is
// derived from it. failing is the write that errored, which may have landed
// anyway: it is restored when the store holds exactly the document this
// rotation sent. A key that moved since this rotation wrote it is someone
// else's write and is never overwritten. The store keeps every version.
func restore(ctx context.Context, store Store, written []Written, done []pendingWrite, failing *pendingWrite, cause error) ([]Written, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), restoreTimeout)
	defer cancel()
	ours := map[string]string{}
	for _, write := range written {
		ours[write.Key] = write.Version
	}
	var failed []error
	var stranded []string
	restored := 0
	restoreOne := func(write *pendingWrite, version string, known bool) {
		strand := func(err error) {
			failed, stranded = append(failed, fmt.Errorf("restore %s: %w", write.key, err)), append(stranded, write.key)
		}
		current, err := store.Describe(ctx, write.key)
		if err != nil {
			strand(err)
			return
		}
		switch {
		case !known && current.Exists == !write.create && current.Version == write.version:
			return // the failed write did not land
		case !known:
			document, readErr := store.Read(ctx, write.key)
			if readErr != nil {
				strand(readErr)
				return
			}
			if !maps.Equal(document, write.document) {
				return // not this rotation's document: someone else's, left alone
			}
		case current.Version != version:
			strand(fmt.Errorf("version %q is no longer the newest (now %q): it was written after this rotation", version, current.Version))
			return
		}
		previous := write.previous
		if previous == nil {
			previous = map[string]string{}
		}
		restoredVersion, err := store.Write(ctx, write.key, previous, false)
		if err != nil {
			strand(err)
			return
		}
		restored++
		written = append(written, Written{Key: write.key, Properties: write.changed, Version: restoredVersion, Restored: true})
	}
	if failing != nil {
		restoreOne(failing, "", false)
	}
	for i := len(done) - 1; i >= 0; i-- {
		version, known := ours[done[i].key]
		restoreOne(&done[i], version, known && version != "")
	}
	if len(failed) > 0 {
		// A re-run would not converge from here: the rotated value is no longer
		// the old one the derived values embed. The previous versions are kept.
		return written, fmt.Errorf("%w; %s may still hold this rotation's documents and could not be restored — re-add each one's previous version as its current version before rotating again: %w",
			cause, strings.Join(stranded, ", "), errors.Join(failed...))
	}
	if restored == 0 {
		return written, fmt.Errorf("%w; nothing this rotation wrote is left in the store", cause)
	}
	return written, fmt.Errorf("%w; the %d remote keys this rotation wrote were restored to their previous documents as new versions, so the store holds no half-rotated value", cause, restored)
}
