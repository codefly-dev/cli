// Package usagelint checks a service against the usage contracts of what it
// consumes: its own agent and the agent of every service it depends on.
//
// Each of those agents may publish a usage contract (codefly/usage/v1) — the
// rules for code that consumes the services it manages. This package gathers
// the contracts over the service's declared dependencies, asks the service's
// own language plugin for its semantic index, and reports where a rule's
// detect clause matches. The contract and the matching are core's
// (contracts/usage); the source is read by the plugin. Nothing here parses a
// contract or a source file itself.
package usagelint

import (
	"context"
	"fmt"
	"io"
	"slices"
	"sort"

	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/agents/contract"
	"github.com/codefly-dev/core/agents/manager"
	usagecontract "github.com/codefly-dev/core/contracts/usage"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	codev0 "github.com/codefly-dev/core/generated/go/codefly/services/code/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
)

// Source is one owner's usage contract as its agent advertised it.
type Source struct {
	// Owner identifies the agent, publisher/name:version.
	Owner string
	// Through says why the contract applies: the service's own agent, or the
	// dependency that brings it.
	Through string
	// Document is the contract, verbatim.
	Document string
}

// Finding is one violation, with what the owner says about it.
type Finding struct {
	usagecontract.Finding
	// Owner is the agent whose rule this is.
	Owner string `json:"owner"`
	// Title, Rationale, Do and Docs are the rule's own words.
	Title     string `json:"title"`
	Rationale string `json:"rationale"`
	Do        string `json:"do"`
	Docs      string `json:"docs,omitempty"`
}

// Owner is one contract that was checked.
type Owner struct {
	Owner   string `json:"owner"`
	Through string `json:"through"`
	Rules   int    `json:"rules"`
	// Detectable is how many of those rules carry a detect clause for a
	// language found in the service. The rest apply as text only.
	Detectable int `json:"detectable"`
}

// Report is what a check found.
type Report struct {
	Service string `json:"service"`
	// Owners are the contracts checked, in the order they were gathered.
	Owners []Owner `json:"owners"`
	// Incomplete says what the plugin could not read: a source file it failed
	// to inspect, or a kind of place its analyzer does not project. While it
	// is set, finding nothing is not a clean bill, and Unverified says
	// whether that fails the check.
	Incomplete []string  `json:"incomplete,omitempty"`
	Findings   []Finding `json:"findings"`
	Errors     int       `json:"errors"`
	Warnings   int       `json:"warnings"`

	unknownLanguageCoverage bool
}

// Unverified is whether the check could not vouch for the service: something
// went unread, and a rule here has a detect clause that could have matched
// in it. A service whose contracts detect nothing in its languages loses
// nothing to an incomplete index, so it is not failed for one.
func (report *Report) Unverified() bool {
	if report.unknownLanguageCoverage {
		return true
	}
	if len(report.Incomplete) == 0 {
		return false
	}
	for _, owner := range report.Owners {
		if owner.Detectable > 0 {
			return true
		}
	}
	return false
}

// Check matches every contract against one semantic index. A contract its
// reader refuses fails the whole check, naming its owner: a rule set that
// cannot be read is not an empty one.
func Check(service string, sources []Source, index *basev0.SemanticIndex) (*Report, error) {
	report := &Report{Service: service, Owners: []Owner{}, Findings: []Finding{}}
	if len(sources) == 0 {
		return report, nil
	}
	languages := index.GetLanguages()
	unknownLanguage := false
	if index.GetState() != basev0.SemanticIndexState_SEMANTIC_INDEX_STATE_COMPLETE {
		unknownLanguage = len(languages) == 0
		report.Incomplete = append(report.Incomplete, fmt.Sprintf("the semantic index is %s", index.GetState()))
		for _, issue := range index.GetIssues() {
			report.Incomplete = append(report.Incomplete, fmt.Sprintf("%s: %s", issue.GetPath(), issue.GetMessage()))
			if !slices.ContainsFunc(index.GetFiles(), func(file *basev0.SemanticFile) bool {
				return file.GetPath() == issue.GetPath() && file.GetLanguage() != ""
			}) {
				unknownLanguage = true
			}
		}
	}
	for _, source := range sources {
		contract, err := usagecontract.Parse([]byte(source.Document))
		if err != nil {
			return nil, fmt.Errorf("usage contract of %s: %w", source.Owner, err)
		}
		owner := Owner{Owner: source.Owner, Through: source.Through, Rules: len(contract.Rules)}
		for i := range contract.Rules {
			rule := &contract.Rules[i]
			if unknownLanguage && len(rule.Detect) > 0 {
				report.unknownLanguageCoverage = true
			}
			for _, language := range languages {
				if rule.Detects(language) != nil {
					owner.Detectable++
					break
				}
			}
		}
		report.Owners = append(report.Owners, owner)
		// What this index left unread for this contract is the same sentence
		// whichever owner asks, so it is reported once.
		for _, gap := range contract.Unread(index) {
			if !slices.Contains(report.Incomplete, gap) {
				report.Incomplete = append(report.Incomplete, gap)
			}
		}
		for _, found := range contract.Detect(index) {
			rule := contract.Rule(found.RuleID)
			report.Findings = append(report.Findings, Finding{
				Finding: found, Owner: source.Owner,
				Title: rule.Title, Rationale: rule.Rationale, Do: rule.Do, Docs: rule.Docs,
			})
			if found.Severity == usagecontract.SeverityError {
				report.Errors++
			} else {
				report.Warnings++
			}
		}
	}
	sort.SliceStable(report.Findings, func(i, j int) bool {
		a, b := report.Findings[i], report.Findings[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Line < b.Line
	})
	return report, nil
}

// Run gathers the contracts that apply to a service and checks it.
func Run(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service) (*Report, error) {
	if service == nil || service.Agent == nil {
		return nil, fmt.Errorf("usage lint requires a service with an agent")
	}
	selection := *service.Agent
	if _, err := manager.ResolveLatest(ctx, &selection); err != nil {
		return nil, fmt.Errorf("resolve service plugin: %w", err)
	}
	logReader, logWriter := io.Pipe()
	logDone := make(chan struct{})
	defer func() {
		_ = logWriter.Close()
		<-logDone
	}()
	go func() {
		defer close(logDone)
		defer logReader.Close()
		agents.GetLogHandler().ForwardLogs(logReader)
	}()
	conn, err := manager.Load(ctx, &selection, manager.WithWorkDir(service.Dir()), manager.WithLogWriter(logWriter), manager.WithoutSandbox(), manager.WithoutPrincipal())
	if err != nil {
		return nil, fmt.Errorf("load service plugin: %w", err)
	}
	defer conn.Close()
	info, err := agentv0.NewAgentClient(conn.GRPCConn()).GetAgentInformation(ctx, &agentv0.AgentInformationRequest{})
	if err != nil {
		return nil, fmt.Errorf("inspect service plugin: %w", err)
	}
	if err = contract.Check(info.GetContract()); err != nil {
		return nil, fmt.Errorf("admit service plugin: %w", err)
	}
	sources, err := gather(ctx, workspace, module, service, &selection, info)
	if err != nil {
		return nil, err
	}
	if len(sources) == 0 {
		return Check(resources.WithUnique(service).Unique(), sources, nil)
	}
	// The Code endpoint resolves the declared source. Usage lint also applies
	// to services whose runtime has no listener.
	client := codev0.NewCodeClient(conn.GRPCConn())
	response, err := client.Execute(ctx, &codev0.CodeRequest{Operation: &codev0.CodeRequest_GetSemanticIndex{GetSemanticIndex: &codev0.GetSemanticIndexRequest{}}})
	if err != nil {
		return nil, fmt.Errorf("semantic index of %s: %w", service.Name, err)
	}
	index := response.GetGetSemanticIndex()
	if index == nil {
		if failure := response.GetFailure(); failure != nil {
			return nil, fmt.Errorf("semantic index of %s: %s", service.Name, failure.GetMessage())
		}
		return nil, fmt.Errorf("the plugin of %s returned no semantic index, so its source cannot be checked", service.Name)
	}
	return Check(resources.WithUnique(service).Unique(), sources, index)
}

// gather collects the usage contract of the service's own agent and of the
// agent of every workspace service it declares a dependency on. An agent
// advertising none contributes nothing. Each agent is asked once.
func gather(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service, own *resources.Agent, info *agentv0.AgentInformation) ([]Source, error) {
	var sources []Source
	seen := map[string]bool{}
	add := func(agent *resources.Agent, through, document string) {
		if agent == nil || document == "" || seen[agent.Identifier()] {
			return
		}
		seen[agent.Identifier()] = true
		sources = append(sources, Source{Owner: agent.Identifier(), Through: through, Document: document})
	}
	add(own, "its own agent", info.GetUsageContract())
	for _, dependency := range service.ServiceDependencies {
		// External and interface-only edges do not select a workspace agent.
		if dependency.Name == "" || dependency.Kind == resources.DependencyKindExternal {
			continue
		}
		owner := module
		if dependency.Module != "" && dependency.Module != module.Name {
			var err error
			if owner, err = workspace.LoadModuleFromName(ctx, dependency.Module); err != nil {
				return nil, fmt.Errorf("dependency %s/%s: %w", dependency.Module, dependency.Name, err)
			}
		}
		depended, err := owner.LoadServiceFromName(ctx, dependency.Name)
		if err != nil {
			return nil, fmt.Errorf("dependency %s/%s: %w", owner.Name, dependency.Name, err)
		}
		if depended.Agent == nil || seen[depended.Agent.Identifier()] {
			continue
		}
		instance, err := services.Load(ctx, workspace, owner, depended)
		if err != nil {
			return nil, fmt.Errorf("load the agent of dependency %s/%s: %w", owner.Name, dependency.Name, err)
		}
		add(depended.Agent, fmt.Sprintf("dependency %s/%s", owner.Name, dependency.Name), instance.Info.GetUsageContract())
	}
	return sources, nil
}
