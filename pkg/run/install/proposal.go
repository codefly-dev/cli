// Package install derives what a composition would have to be granted, from
// what it declares.
//
// Accounts' InstallSolution produces one agent principal, one solution scope
// node, one standing role grant and one organization installation, and its
// installer surface is four paths: token, inspect, apply and verify. The
// envelope that install is approved against is deliberately NOT derived from a
// module's own declaration — MODULE_INSTALLATION.md: "Policy publication is an
// explicit trust-root operation; an ordinary module release declaration cannot
// write this projection." A module that could write its own envelope could
// grant itself authority by declaring it.
//
// So this derives the REQUEST and never the approval. It reads the
// composition's manifests and says what an approver would be deciding about:
// which services exist, which workspace-configuration groups each reads, and
// which endpoints each calls. It grants nothing, signs nothing, and reaches no
// network. The decision stays where the trust root put it.
package install

import (
	"fmt"
	"sort"
	"strings"
)

// Service is one service's declaration, as the composition states it.
type Service struct {
	Module string
	Name   string
	// Groups are the workspace-configuration groups it reads. Each is a
	// capability the composition supplies, and each is a way for the service to
	// be unable to start without one.
	Groups []string
	// Dependencies are the endpoints it calls, as "module/service/endpoint".
	Dependencies []string
}

// Need is what one service would have to be granted for the composition to run.
type Need struct {
	Service Service
	// Reaches is what it calls outside its own module. A dependency inside the
	// module is composed, not granted: the authority question is about crossing
	// the boundary.
	Reaches []string
}

// Unique names the service the way the rest of codefly does.
func (s Service) Unique() string { return s.Module + "/" + s.Name }

// Derive turns the composition's declarations into the needs an approver would
// decide about. It is pure: the same declarations always give the same needs,
// so what an operator reads in a deployed environment is what a local run
// derived from the same manifests.
func Derive(services []Service) []Need {
	owned := map[string]bool{}
	for _, service := range services {
		owned[service.Module] = true
	}
	needs := make([]Need, 0, len(services))
	for _, service := range services {
		need := Need{Service: service}
		for _, dependency := range service.Dependencies {
			module, _, found := strings.Cut(dependency, "/")
			// A dependency this composition does not contain is the one that
			// has to be granted: it crosses out of what the operator is
			// installing.
			if !found || !owned[module] {
				need.Reaches = append(need.Reaches, dependency)
			}
		}
		sort.Strings(need.Reaches)
		needs = append(needs, need)
	}
	sort.Slice(needs, func(i, j int) bool { return needs[i].Service.Unique() < needs[j].Service.Unique() })
	return needs
}

// Render writes the needs as the text an approver reads before deciding.
//
// It names what is being asked for and says plainly that nothing here grants
// it, because a list of requirements is easy to mistake for a list of things
// that have been arranged.
func Render(needs []Need) string {
	if len(needs) == 0 {
		return "This composition declares no services, so there is nothing to install.\n"
	}
	var out strings.Builder
	out.WriteString("This composition declares the following. Nothing below is granted by deriving it:\n\n")
	for _, need := range needs {
		fmt.Fprintf(&out, "  %s\n", need.Service.Unique())
		if len(need.Service.Groups) > 0 {
			fmt.Fprintf(&out, "    reads configuration: %s\n", strings.Join(need.Service.Groups, ", "))
		}
		if len(need.Reaches) > 0 {
			fmt.Fprintf(&out, "    calls outside this composition: %s\n", strings.Join(need.Reaches, ", "))
		}
		if len(need.Service.Groups) == 0 && len(need.Reaches) == 0 {
			out.WriteString("    reads no configuration and calls nothing outside this composition\n")
		}
	}
	out.WriteString("\nAn envelope approving any of it is a trust-root operation and is not written here.\n")
	return out.String()
}
