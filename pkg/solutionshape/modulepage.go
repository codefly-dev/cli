package solutionshape

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/codefly-dev/core/composition"
	modulecontract "github.com/codefly-dev/core/contracts/module"
	"github.com/codefly-dev/core/solutionhost"
)

// A module page is a module that ships no solution manifest and declares a
// page beside one of its services. People reach it, so a host offers it the
// way it offers a solution: an administrator sets up its roles for teams, and
// a person is entitled to the page only through a role it declares. Until
// this check, a module page was "not a solution" here and judged on nothing:
// a page declaring no roles, or roles no operation needs, read as fine.

const (
	// CodeModulePageWithoutRoles names a module page whose contract declares
	// no role, so nobody can ever be granted the page.
	CodeModulePageWithoutRoles = "module_page_declares_no_roles"
)

// modulePageDeclaration is the page declaration of the module at dir,
// relative to it, or "" when it declares none: a frontend file beside any of
// its services.
func modulePageDeclaration(dir string) string {
	matches, _ := filepath.Glob(filepath.Join(dir, "services", "*", FrontendFileName))
	if len(matches) == 0 {
		return ""
	}
	relative, err := filepath.Rel(dir, matches[0])
	if err != nil {
		return ""
	}
	return relative
}

// inspectModulePage judges a module page on what decides who can use it: the
// page declaration, the roles a person is granted, the installation's standing
// authority, and what each role reaches.
func (r *Report) inspectModulePage(dir, page string) {
	r.IsModulePage = true
	raw, err := os.ReadFile(filepath.Join(dir, page))
	if err == nil {
		_, err = solutionhost.ParseFrontend(raw)
	}
	if err != nil {
		r.fail(CodeFrontendUnreadable, fmt.Sprintf("%s is refused by core's parser: %v", page, err),
			"this is the same reader that refuses it at delivery, so the module would be delivered and its page never mounted")
		return
	}
	r.ok(CodeFrontendMissing, fmt.Sprintf("the module declares a page (%s)", page))

	if _, statErr := os.Stat(filepath.Join(dir, modulecontract.FileName)); errors.Is(statErr, os.ErrNotExist) {
		r.fail(CodeContractMissing, fmt.Sprintf("no %s", modulecontract.FileName),
			"a person reaches a module page only through a role its contract declares; with no contract there is no role to grant, and the page is shown to nobody")
		return
	}
	contract, err := modulecontract.Load(dir)
	if err != nil {
		r.fail(CodeContractUnreadable, fmt.Sprintf("cannot read %s: %v", modulecontract.FileName, err), "")
		return
	}
	r.ok(CodeContractMissing, fmt.Sprintf("the contract declares principal %q", contract.Principal))
	if len(contract.Roles) == 0 {
		r.fail(CodeModulePageWithoutRoles, "the module declares a page and no role",
			"entitlement to a page is holding one of its roles through a team, so a page with none is shown to nobody and offers an administrator nothing to set up. Declare installer, editor and viewer within the contract's scope_ceilings")
		return
	}
	r.ok(CodeModulePageWithoutRoles, fmt.Sprintf("the module page declares %d role(s)", len(contract.Roles)))
	r.checkInstallerRole(contract)
	r.checkInstallationRole(contract, true)

	if _, statErr := os.Stat(filepath.Join(dir, composition.PackageManifestFileName)); errors.Is(statErr, os.ErrNotExist) {
		r.fail(CodePackageMissing, fmt.Sprintf("no %s", composition.PackageManifestFileName),
			"delivery refuses a module page that declares no package manifest, and its operations are read from the contracts the package names")
		return
	}
	packaged, err := composition.LoadPackageManifest(dir)
	if err != nil {
		r.fail(CodePackageUnreadable, fmt.Sprintf("cannot read %s: %v", composition.PackageManifestFileName, err), "")
		return
	}
	r.ok(CodePackageMissing, fmt.Sprintf("the package publishes %s@%s", packaged.ID, packaged.Version))
	if !packageDeclaresAContract(packaged) {
		r.warn(CodeOperationPoliciesUnread, "the package carries no published contract, so what the roles reach cannot be judged",
			"the authority derives every operation's policy from the published contracts. Declare \"contracts\" under artifact-roots and run `codefly generate contracts`")
	}
	r.judgeRoles(contract, r.checkRoleScopesAreReached(dir, contract, packaged))
}
