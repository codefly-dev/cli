package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/codefly-dev/authority/libraries/authority-core/go/decide"
	"github.com/codefly-dev/authority/libraries/authority-core/go/domain"
	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/core/resources"
	"github.com/spf13/cobra"
)

var authorityFlags struct {
	root, env, moduleDir, caller, binding, method, role string
	failUnproven                                        bool
}

// AuthorityCmd groups the read-only answers about what declarations admit.
var AuthorityCmd = &cobra.Command{
	Use:   "authority",
	Short: "Check what the authority's declarations admit, and explain why a call is refused",
}

var authorityCheckCmd = &cobra.Command{
	Use:          "check",
	SilenceUsage: true,
	Short:        "Fail when declarations that must agree do not: roles, ceilings, the envelope, generated policy tables",
	Long: `A static gate over committed files: no cluster, no credential, no network.

--env <environment>, in a composition, reads what that environment delivers to
its host (each module's paired delivery document, and the module envelope) and
fails when a module's declaration does not hold to its own contract (a role
granting beyond its ceiling refuses the whole delivery), when a role names a
scope the composition reaches nowhere, or when the envelope withholds a scope
a binding asks for: the host drops that scope without a record, so the first
symptom is a refused call.

--module-dir <module>, in a module's checkout, fails when a generated
permissions table (pkg/gen/permissions/permissions_gen.go) disagrees with the
method policies the module publishes: a service's own tests then pass against
the stale side.

With both, the checkout is also held to what the environment delivers for
the same module: a branch that moved without a delivery is a refused copy.

A method open to any checked caller on a module that checks owners is listed
UNPROVEN, every one, with no exemption: its handler decides and no declaration
states what it asks, so declarations agreeing proves nothing about it. With
any unproven the command does not print "ok"; --fail-on-unproven makes it
exit 1. Agreement is not sufficiency: only calling the handler proves that.

Every finding names the file and the field. A file that cannot be read is a
finding, never a pass.`,
	Example: "codefly authority check --env blue\ncodefly authority check --module-dir modules/record-store",
	RunE: func(command *cobra.Command, _ []string) error {
		if authorityFlags.env == "" && authorityFlags.moduleDir == "" {
			return errors.New("name what to check: --env <environment>, --module-dir <module>, or both")
		}
		var findings, unproven []string
		var delivered *gitops.DeliveredAuthority
		if authorityFlags.env != "" {
			var err error
			if delivered, err = readDeliveredAuthority(authorityFlags.root, authorityFlags.env); err != nil {
				return err
			}
			findings = append(findings, deliveredFindings(delivered)...)
			for _, principal := range slices.Sorted(maps.Keys(delivered.Modules)) {
				if entry := delivered.Modules[principal]; entry.Unread == "" {
					unproven = append(unproven, unstated(entry.File, &entry.Module)...)
				}
			}
		}
		if dir := authorityFlags.moduleDir; dir != "" {
			findings = append(findings, tableFindings(dir)...)
			principal, ownerAware, err := gitops.OwnDeclaration(dir)
			published, publishedErr := gitops.PublishedPolicies(dir)
			switch {
			case err != nil:
				findings = append(findings, fmt.Sprintf("%s: its module contract could not be read, so whether it checks owners is unknown: %v", dir, err))
			case publishedErr == nil:
				unproven = append(unproven, unstated(dir, &domain.Module{Name: principal, Principal: principal, OwnerAware: ownerAware, Methods: published})...)
			}
			if delivered != nil && err == nil {
				findings = append(findings, undeliveredFindings(delivered, principal, dir)...)
			}
		}
		out := command.OutOrStdout()
		for _, finding := range findings {
			fmt.Fprintln(out, "FAIL "+finding)
		}
		for _, link := range unproven {
			fmt.Fprintln(out, "UNPROVEN "+link)
		}
		if len(unproven) > 0 {
			fmt.Fprintf(out, "%d method(s) unproven: agreement between declarations is not proof the handler asks what they say. Not a pass.\n", len(unproven))
		}
		switch {
		case len(findings) > 0:
			return fmt.Errorf("%d authority declaration(s) disagree", len(findings))
		case len(unproven) > 0 && authorityFlags.failUnproven:
			return fmt.Errorf("%d method(s) unproven", len(unproven))
		case len(unproven) == 0:
			fmt.Fprintln(out, "ok: every declaration read agrees")
		}
		return nil
	},
}

// unstated is every method of m whose owner link no declaration decides
// (decide.Trace's own verdict): open to any checked caller on a module that
// checks owners. It is listed whatever the method is; nothing is exempt.
func unstated(where string, m *domain.Module) []string {
	var out []string
	for _, method := range slices.Sorted(maps.Keys(m.Methods)) {
		for _, link := range decide.Trace(decide.Call{Audience: *m, Method: method}) {
			if link.Name == decide.LinkOwner && link.Verdict == decide.Unproven {
				out = append(out, fmt.Sprintf("%s: methods[%s]: %s", where, method, link.Reason))
			}
		}
	}
	return out
}

// undeliveredFindings holds the checkout at dir to what the environment
// delivers for the same module, method by method: a checkout that moved
// without a delivery, or a delivery the checkout never held, is a refused copy.
func undeliveredFindings(d *gitops.DeliveredAuthority, principal, dir string) []string {
	entry, held := d.Modules[principal]
	if !held || entry.Unread != "" {
		return []string{fmt.Sprintf("%s: the environment delivers no readable module %q to hold this checkout to (%s %s)", dir, principal, entry.File, entry.Unread)}
	}
	of, _ := policyCopies(dir)
	var out []string
	for _, method := range slices.Sorted(maps.Keys(entry.Module.Methods)) {
		for _, link := range decide.Copies(entry.Module.Methods[method], true, of(method)) {
			if link.Verdict != decide.Admitted {
				out = append(out, fmt.Sprintf("%s: %s: %s (delivered by %s)", link.Field, method, link.Reason, entry.File))
			}
		}
	}
	return out
}

// deliveredFindings is every disagreement in what an environment delivers.
func deliveredFindings(d *gitops.DeliveredAuthority) []string {
	var out []string
	if d.EnvelopeUnread != "" {
		out = append(out, fmt.Sprintf("%s: the module envelope could not be read, so no binding is known to be granted: %s", d.EnvelopeFile, d.EnvelopeUnread))
	}
	composed := map[string]domain.Module{}
	for principal := range d.Modules {
		if d.Modules[principal].Unread == "" {
			composed[principal] = d.Modules[principal].Module
		}
	}
	for _, principal := range slices.Sorted(maps.Keys(d.Modules)) {
		entry := d.Modules[principal]
		if entry.Unread != "" {
			out = append(out, fmt.Sprintf("%s: could not be read as a delivery: %s", entry.File, entry.Unread))
			continue
		}
		m := entry.Module
		for _, err := range []error{decide.ValidateModule(m), decide.Unreached(m, composed)} {
			if err != nil {
				out = append(out, fmt.Sprintf("%s: %v", entry.File, err))
			}
		}
		if d.EnvelopeUnread != "" {
			continue
		}
		for _, id := range slices.Sorted(maps.Keys(m.Bindings)) {
			field := fmt.Sprintf("%s: principals[%s].operation_audiences[%s]", d.EnvelopeFile, principal, id)
			asked := fmt.Sprintf("which %s asks for in authority.bindings[%s]", entry.File, id)
			allowed, named := d.Envelope[principal][id]
			if !named {
				out = append(out, fmt.Sprintf("%s is absent, %s: the host registers no such binding", field, asked))
				continue
			}
			granted, err := decide.Granted(m.Bindings[id], allowed)
			if err != nil {
				out = append(out, fmt.Sprintf("%s: %v", field, err))
				continue
			}
			for _, withheld := range decide.Withheld(m.Bindings[id], granted) {
				operation, permission, _ := strings.Cut(withheld, " ")
				out = append(out, fmt.Sprintf("%s.%s_scopes withholds %s, %s: the host drops it without a record", field, operation, permission, asked))
			}
		}
	}
	return out
}

// policyTables are the generated permissions tables under a module directory.
func policyTables(dir string) []string {
	var tables []string
	_ = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && entry.IsDir() && (entry.Name() == "node_modules" || entry.Name() == ".git") {
			return filepath.SkipDir
		}
		if err == nil && entry.Name() == "permissions_gen.go" {
			tables = append(tables, path)
		}
		return nil
	})
	return tables
}

// policyCopies reads, once, every written copy of the method policies of the
// module checked out at dir: the contract it publishes, and each generated
// table. of answers one method's copies; methods are those the contract
// publishes, nil when it could not be read.
func policyCopies(dir string) (of func(method string) []decide.Statement, methods []string) {
	type source struct {
		where, unread string
		policies      map[string]domain.MethodPolicy
	}
	read := func(where string, policies map[string]domain.MethodPolicy, err error) source {
		if err != nil {
			return source{where: where, unread: err.Error()}
		}
		return source{where: where, policies: policies}
	}
	published, err := gitops.PublishedPolicies(dir)
	sources := []source{read(filepath.Join(dir, "contracts")+" (the published method policies)", published, err)}
	tables := policyTables(dir)
	if len(tables) == 0 {
		sources = append(sources, source{where: dir, unread: "no generated permissions_gen.go is under this directory"})
	}
	for _, path := range tables {
		table, err := gitops.PermissionTable(path)
		sources = append(sources, read(path, table, err))
	}
	return func(method string) []decide.Statement {
		copies := make([]decide.Statement, 0, len(sources))
		for _, s := range sources {
			policy, declared := s.policies[method]
			copies = append(copies, decide.Statement{Where: s.where, Unread: s.unread, Policy: policy, Declared: declared})
		}
		return copies
	}, slices.Sorted(maps.Keys(published))
}

// tableFindings holds every generated table of the module at dir to the
// policies it publishes, method by method (decide.Copies).
func tableFindings(dir string) []string {
	of, methods := policyCopies(dir)
	var out []string
	if methods == nil {
		methods = []string{""} // the contract is unread: say so once, as its own copy does
	}
	for _, method := range methods {
		copies := of(method)
		for _, link := range decide.Copies(copies[0].Policy, copies[0].Declared, copies) {
			if link.Verdict != decide.Admitted {
				out = append(out, fmt.Sprintf("%s: %s: %s", link.Field, method, link.Reason))
			}
		}
	}
	return out
}

var authorityTraceCmd = &cobra.Command{
	Use:          "trace",
	SilenceUsage: true,
	Short:        "Answer why a call is admitted or refused, link by link, naming the file and field that decide each",
	Long: `Walk the chain one call travels and print every link with its verdict, the
file and field it was read from, the value held there, and the refusing
decision's own reason. It names the first link that does not admit the call
and keeps walking, so everything wrong is shown, not only the earliest.

The call: a holder of --role on --caller's installation (omit --role for the
module acting alone, on its headless scopes) reaching --method of the module
--binding is addressed to.

Static mode, the only one built: links 4 to 8 are read from the committed tree
of --env (the paired delivery documents and the module envelope), with no
cluster. --module-dir <the audience's checkout> adds every other written copy
of the method's policy there: its published contract and generated tables.
Links 1 to 3 (page registered, installed and entitled, role held) need a live
cell and are printed "unread", never as passed.

A verdict is one of: admitted; refused; unread (the source could not be read);
unproven (read, and no declaration states what decides it — the handler of a
module that checks owners). Only "admitted" is a pass. Exit 1 unless links 4
to 8 all admit.`,
	Example: `  codefly authority trace --env blue --caller org-directory --binding records-types \
    --method /records.v1.RecordsService/GetRecordType --module-dir ../module-record-store/modules/record-store
  codefly authority trace --env blue --caller org-directory --role steward --binding records-rows \
    --method /records.v1.RecordsService/ListRecordTypes`,
	RunE: func(command *cobra.Command, _ []string) error {
		f := authorityFlags
		if f.env == "" || f.caller == "" || f.binding == "" || f.method == "" {
			return errors.New("a trace names --env, --caller, --binding and --method")
		}
		d, err := readDeliveredAuthority(f.root, f.env)
		if err != nil {
			return err
		}
		caller, held := d.Modules[f.caller]
		if !held || caller.Unread != "" {
			return fmt.Errorf("the delivery of caller %q could not be read (%s %s), so no link of its chain has a verdict", f.caller, caller.File, caller.Unread)
		}
		audience := d.Modules[caller.Module.Bindings[f.binding].Audience]
		call := decide.Call{Caller: caller.Module, Audience: audience.Module, Role: f.role, Binding: f.binding, Method: f.method,
			// No cell is read, so the installation is a stand-in: its principal only marks which scopes arrive owner-selected.
			Installation: domain.Installation{PrincipalID: "<the installation's principal>"}, EnvelopeUnread: d.EnvelopeUnread}
		if allowed, named := d.Envelope[f.caller][f.binding]; named {
			call.Envelope = &allowed
		}
		if f.moduleDir != "" {
			of, _ := policyCopies(f.moduleDir)
			call.Statements = of(f.method)
		}
		files := map[string]string{decide.LinkRole: caller.File, decide.LinkCeiling: caller.File, decide.LinkEnvelope: d.EnvelopeFile,
			decide.LinkPolicy: audience.File, decide.LinkOwner: audience.File}
		return printTrace(command.OutOrStdout(), &call, decide.Trace(call), files, &audience)
	},
}

// printTrace prints the chain; the links a static read cannot reach are
// printed unread, and an audience whose delivery was not read voids its links.
func printTrace(w io.Writer, call *decide.Call, links []decide.Link, files map[string]string, audience *gitops.DeliveredModule) error {
	fmt.Fprintf(w, "call: %s -> %s through binding %q of %s\n\n", call.Method, call.Audience.Name, call.Binding, call.Caller.Name)
	for i, name := range []string{"page registered", "installed and entitled", "role held"} {
		fmt.Fprintf(w, "%d %-22s UNREAD\n    because  static mode reads no cell: this link is not checked, and is not a pass\n", i+1, name)
	}
	numbers := map[string]int{decide.LinkRole: 4, decide.LinkCeiling: 5, decide.LinkEnvelope: 6, decide.LinkPolicy: 7, decide.LinkPolicyCopy: 7, decide.LinkOwner: 8}
	for i := range links {
		link := &links[i]
		if (link.Name == decide.LinkPolicy || link.Name == decide.LinkOwner) && (audience.File == "" || audience.Unread != "") {
			link.Verdict, link.Value = decide.Unread, ""
			link.Reason = fmt.Sprintf("the delivery of the audience binding %q is addressed to was not read (%s %s)", call.Binding, audience.File, audience.Unread)
		}
		fmt.Fprintf(w, "%d %-22s %s\n", numbers[link.Name], link.Name, strings.ToUpper(link.Verdict))
		file, field := files[link.Name], link.Field
		if link.Name == decide.LinkPolicyCopy {
			file, field = link.Field, "methods["+call.Method+"]"
		}
		for _, row := range [][2]string{{"file", file}, {"field", field}, {"value", link.Value}, {"because", link.Reason}} {
			if row[1] != "" {
				fmt.Fprintf(w, "    %-8s %s\n", row[0], row[1])
			}
		}
	}
	first, refused := decide.First(links)
	if !refused {
		fmt.Fprintln(w, "\nlinks 4-8 admit the call; links 1-3 were not read.")
		return nil
	}
	fmt.Fprintf(w, "\nFIRST LINK THAT DOES NOT ADMIT THE CALL: %d %s (%s) %s %s\n", numbers[first.Name], first.Name, first.Verdict, files[first.Name], first.Field)
	return fmt.Errorf("the call is not admitted: link %d (%s) is %s", numbers[first.Name], first.Name, first.Verdict)
}

func init() {
	for _, command := range []*cobra.Command{authorityCheckCmd, authorityTraceCmd} {
		command.Flags().StringVar(&authorityFlags.root, "root", ".", "the composition's workspace root: the environment's working render is read there, and its delivered tree when the checkout is its delivery repository on its delivery branch")
		command.Flags().StringVar(&authorityFlags.env, "env", "", "the environment whose delivered tree is read")
		command.Flags().StringVar(&authorityFlags.moduleDir, "module-dir", "", "a module's checkout: its published policies and generated tables")
		AuthorityCmd.AddCommand(command)
	}
	authorityCheckCmd.Flags().BoolVar(&authorityFlags.failUnproven, "fail-on-unproven", false, "exit 1 when a method is open on a module that checks owners")
	authorityTraceCmd.Flags().StringVar(&authorityFlags.caller, "caller", "", "the calling module's principal")
	authorityTraceCmd.Flags().StringVar(&authorityFlags.binding, "binding", "", "the caller's binding the call goes through")
	authorityTraceCmd.Flags().StringVar(&authorityFlags.method, "method", "", "the audience's method, as its policy names it")
	authorityTraceCmd.Flags().StringVar(&authorityFlags.role, "role", "", "the role held on the caller's installation; omit for the module acting alone")
}

// readDeliveredAuthority reads what environment delivers from the workspace at
// root: its own module trees only (gitops.EnvironmentModuleTrees), never a
// tree another environment delivers from.
func readDeliveredAuthority(root, environment string) (*gitops.DeliveredAuthority, error) {
	ctx := context.Background()
	workspace, err := resources.LoadWorkspaceFromDir(ctx, root)
	if err != nil {
		return nil, fmt.Errorf("load the workspace at %s: %w", root, err)
	}
	env, err := environments.Select(workspace, environment)
	if err != nil {
		return nil, err
	}
	trees, err := gitops.EnvironmentModuleTrees(ctx, workspace, env)
	if err != nil {
		return nil, err
	}
	return gitops.ReadDeliveredAuthority(trees, environment)
}
