package gitops

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/codefly-dev/cli/pkg/environments"
)

// A deployed workload's identity is what everything downstream has to name to
// tell two workloads apart — an authorization rule, a network identity and a
// cloud identity binding all name a principal — so a principal shared by two
// workloads grants each of them whatever either was granted. The account
// Kubernetes gives a pod that names none is shared by every pod that names
// none, so it is never an identity.
//
// The rule is therefore: a workload runs under an account its own service's
// render DEFINES. That is resolved against the effective render rather than
// compared to the service's name, because the name in the output is not the name
// in the source — a kustomize transform renames an account and every reference
// to it together, and both are still that service's own identity. What the
// render has to establish is that the identity exists in this unit, not that it
// is spelled a particular way.
//
// Everything is read from the effective render: what a cluster would receive
// after the environment's overlay is built. A file's name, the directory it sits
// in and a patch applied after the render all count.

// workloadAccount is one effective workload and the principal it runs as.
type workloadAccount struct {
	kind      string
	name      string
	namespace string
	account   string
}

func (workload workloadAccount) String() string {
	return workload.kind + "/" + workload.name
}

// effectiveUnit is one rendered service unit as a cluster would receive it: the
// workloads it runs, and the service accounts it defines for them.
type effectiveUnit struct {
	workloads []workloadAccount
	// accounts is the set of "<namespace>/<name>" this unit defines.
	accounts map[string]bool
}

// defines reports whether this unit defines the account a workload binds.
func (unit effectiveUnit) defines(workload workloadAccount) bool {
	return unit.accounts[workload.namespace+"/"+workload.account]
}

// definedAccounts lists what the unit defines, for a refusal to name.
func (unit effectiveUnit) definedAccounts() string {
	if len(unit.accounts) == 0 {
		return "none"
	}
	named := make([]string, 0, len(unit.accounts))
	for account := range unit.accounts {
		named = append(named, account)
	}
	sort.Strings(named)
	return strings.Join(named, ", ")
}

// readEffectiveUnit reads one rendered service unit as a cluster would receive
// it, through overlayManifests — the one built-overlay reader this package has,
// so this rule and everything else decided on a rendered unit read the same
// bytes.
//
// It keeps the account EXACTLY as the pod template names it, empty included,
// because the write decision depends on the difference: a template that names
// none can be given one, and a template that names "default" has already
// answered. The refusal does not draw that distinction — a pod that names none
// and a pod that names "default" both run as the account every pod of the
// namespace shares.
func readEffectiveUnit(unitDir, environment string) (effectiveUnit, error) {
	// An environment's name is one path component — it names a directory beside
	// the unit's base, not a path into one — so it is reduced to its base before
	// it is joined. A name that arrived from a flag cannot then decide where the
	// render looks.
	documents, err := overlayManifests(filepath.Join(unitDir, "overlays", filepath.Base(environment)))
	if err != nil {
		return effectiveUnit{}, err
	}
	unit := effectiveUnit{accounts: map[string]bool{}}
	for _, document := range documents {
		if document.group == "" && document.kind == "ServiceAccount" {
			unit.accounts[metadataString(document.value, "namespace")+"/"+metadataString(document.value, "name")] = true
			continue
		}
		spec, ok := podSpec(document)
		if !ok {
			continue
		}
		account, _ := spec["serviceAccountName"].(string)
		unit.workloads = append(unit.workloads, workloadAccount{
			kind:      document.kind,
			name:      metadataString(document.value, "name"),
			namespace: metadataString(document.value, "namespace"),
			account:   strings.TrimSpace(account),
		})
	}
	return unit, nil
}

// requireOwnServiceAccount refuses an effective render in which a workload runs
// under no account, under the shared default, or under one this service's own
// render does not define.
func requireOwnServiceAccount(unitDir, environment, module, service string) error {
	unit, err := readEffectiveUnit(unitDir, environment)
	if err != nil {
		return err
	}
	var refusals []string
	for _, workload := range unit.workloads {
		switch {
		case workload.account == "" || workload.account == defaultServiceAccount:
			refusals = append(refusals, fmt.Sprintf(
				"%s runs under the account every pod of its namespace shares, which is no identity at all", workload))
		case !unit.defines(workload):
			refusals = append(refusals, fmt.Sprintf(
				"%s runs under account %q, which this service's render does not define (it defines: %s), so the identity belongs to something else",
				workload, workload.account, unit.definedAccounts()))
		}
	}
	if len(refusals) == 0 {
		return nil
	}
	sort.Strings(refusals)
	return fmt.Errorf("the effective render of service %s/%s binds a workload to an identity that is not its own:\n  %s",
		module, service, strings.Join(refusals, "\n  "))
}

// accountState is what the effective render already says about one service's
// identity, read before anything is written.
type accountState int

const (
	// accountIsUnbound: a workload names no account, and the writer can reach it.
	accountIsUnbound accountState = iota
	// accountIsItsOwn: every workload binds an account this unit defines.
	accountIsItsOwn
	// accountHasNoWorkload: the unit renders no pod at all.
	accountHasNoWorkload
	// accountIsSomeoneElses: a workload binds the shared default or an account
	// this unit does not define, or binds nothing in a tree the writer cannot
	// reach. Writing cannot fix any of those, so the render does not try.
	accountIsSomeoneElses
)

// deployedAccountState reads the effective render and says which of those four
// it is.
func deployedAccountState(unitDir, baseDir, environment string) (accountState, error) {
	unit, err := readEffectiveUnit(unitDir, environment)
	if err != nil {
		return accountIsSomeoneElses, err
	}
	if len(unit.workloads) == 0 {
		return accountHasNoWorkload, nil
	}
	own := 0
	for _, workload := range unit.workloads {
		switch {
		case workload.account == "":
		case workload.account != defaultServiceAccount && unit.defines(workload):
			own++
		default:
			return accountIsSomeoneElses, nil
		}
	}
	if own == len(unit.workloads) {
		return accountIsItsOwn, nil
	}
	// Something is unbound. The writer reads the YAML files directly in base, so
	// a tree whose workload it cannot reach cannot be fixed by writing.
	writable, err := baseBindsAWorkload(baseDir)
	if err != nil {
		return accountIsSomeoneElses, err
	}
	if !writable {
		return accountIsSomeoneElses, nil
	}
	return accountIsUnbound, nil
}

// baseBindsAWorkload reports whether the account writer can act on this base
// tree at all: it reads the YAML files directly in base, so a workload reached
// through a differently-suffixed file or a nested resource directory is one it
// cannot write to. Asking first is what lets such a tree reach the refusal —
// which names the workload and the account it runs under — instead of the
// writer's own "bound no workload" error, which says nothing about the identity
// at stake.
func baseBindsAWorkload(baseDir string) (bool, error) {
	entries, err := os.ReadDir(baseDir)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read service base: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(baseDir, entry.Name()))
		if err != nil {
			return false, fmt.Errorf("read %s: %w", entry.Name(), err)
		}
		documents, _, err := decodeYAML(entry.Name(), data)
		if err != nil {
			return false, fmt.Errorf("parse %s: %w", entry.Name(), err)
		}
		for _, document := range documents {
			if _, ok := podSpec(document); ok {
				return true, nil
			}
		}
	}
	return false, nil
}

// namespaceAccounts is every (namespace, account) a render bound, and which
// service bound it.
type namespaceAccounts map[string]string

// record claims one account in one namespace for one service, refusing a second
// service that claims the same one.
func (claimed namespaceAccounts) record(namespace, account, service string) error {
	if account == "" {
		return nil
	}
	key := namespace + "/" + account
	if holder, taken := claimed[key]; taken && holder != service {
		return fmt.Errorf(
			"services %s and %s both bind the service account %q in namespace %q; a principal two workloads share grants each of them whatever either was granted",
			holder, service, account, namespace)
	}
	claimed[key] = service
	return nil
}

// recordUnit claims every principal one rendered service unit binds, so two
// services of one render cannot end up on the same one. It reads the effective
// render, like everything else about identity here.
func (claimed namespaceAccounts) recordUnit(unitDir string, env *environments.Environment, service string) error {
	if !env.Deployed() {
		return nil
	}
	unit, err := readEffectiveUnit(unitDir, env.Name)
	if err != nil {
		return fmt.Errorf("read service %s workloads: %w", service, err)
	}
	for _, workload := range unit.workloads {
		if err := claimed.record(workload.namespace, workload.account, service); err != nil {
			return err
		}
	}
	return nil
}
