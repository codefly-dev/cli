// Package composition resolves a consumer's transitive library-dependency
// closure and checks that it is coherent: every shared library resolves to a
// single major version. When two independently generated SDKs share a
// first-party contract (a common proto library) at incompatible majors, the
// consumer that installs both fails to link — two distinct Go types register
// the same proto file path and panic at init, Python hits a duplicate symbol
// in the descriptor pool, TS duplicates types. Catching the diamond here, at
// resolution time, turns that runtime panic into a clear configuration error.
package composition

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/Masterminds/semver"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
)

// versionResolver resolves a library name and semver constraint to the
// concrete library and the version that satisfies it.
// *resources.LibraryResolver is the production implementation.
type versionResolver interface {
	ResolveVersion(ctx context.Context, name, constraint string) (*resources.Library, string, error)
}

// Resolver walks a consumer's transitive library-dependency graph and reports
// whether the resulting closure is coherent.
type Resolver struct {
	resolve versionResolver
}

// NewResolver builds a Resolver over the workspace's libraries.
func NewResolver(workspace *resources.Workspace) *Resolver {
	return &Resolver{resolve: resources.NewLibraryResolver(workspace)}
}

// Requirement is a single edge into a library within a closure: who asked for
// it, under what constraint, and the version that constraint resolved to.
type Requirement struct {
	RequiredBy string
	Constraint string
	Resolved   string
}

// Closure is the transitive set of libraries a consumer pulls in, keyed by
// library name, with every requirement that referenced each one.
type Closure struct {
	Requirements map[string][]Requirement
}

// Closure resolves the transitive library closure rooted at deps. rootLabel
// names the consumer (e.g. "service backend/api") for diagnostics.
func (r *Resolver) Closure(ctx context.Context, deps []*resources.LibraryDependency, rootLabel string) (*Closure, error) {
	w := wool.Get(ctx).In("composition.Resolver.Closure")
	closure := &Closure{Requirements: map[string][]Requirement{}}

	type edge struct {
		name       string
		constraint string
		requiredBy string
	}
	var queue []edge
	for _, dep := range deps {
		if dep == nil || dep.Name == "" {
			continue
		}
		queue = append(queue, edge{name: dep.Name, constraint: dep.Version, requiredBy: rootLabel})
	}

	// A library reachable through several paths is resolved (and recorded) on
	// each path so every constraint on it is captured, but its own
	// dependencies are only expanded once — that terminates on cycles too.
	expanded := map[string]bool{}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		lib, resolved, err := r.resolve.ResolveVersion(ctx, current.name, current.constraint)
		if err != nil {
			return nil, w.Wrapf(err, "cannot resolve library %s (%s) required by %s", current.name, current.constraint, current.requiredBy)
		}
		closure.Requirements[current.name] = append(closure.Requirements[current.name], Requirement{
			RequiredBy: current.requiredBy,
			Constraint: current.constraint,
			Resolved:   resolved,
		})

		if expanded[current.name] {
			continue
		}
		expanded[current.name] = true
		for _, dep := range lib.LibraryDeps {
			if dep == nil || dep.Name == "" {
				continue
			}
			queue = append(queue, edge{name: dep.Name, constraint: dep.Version, requiredBy: "library " + lib.Name})
		}
	}
	return closure, nil
}

// MajorRequirements groups the requirements on a library that resolved to the
// same major version.
type MajorRequirements struct {
	Major        int64
	Requirements []Requirement
}

// Violation is a library that a closure requires at more than one major
// version — a diamond that cannot be satisfied by a single installed copy.
type Violation struct {
	Library string
	Majors  []MajorRequirements
}

// Violations returns every shared library the closure requires at more than
// one major version, sorted by library name.
func (c *Closure) Violations() ([]Violation, error) {
	names := make([]string, 0, len(c.Requirements))
	for name := range c.Requirements {
		names = append(names, name)
	}
	sort.Strings(names)

	var violations []Violation
	for _, name := range names {
		byMajor := map[int64][]Requirement{}
		for _, req := range c.Requirements[name] {
			v, err := semver.NewVersion(req.Resolved)
			if err != nil {
				return nil, fmt.Errorf("library %s resolved to invalid version %q: %w", name, req.Resolved, err)
			}
			byMajor[v.Major()] = append(byMajor[v.Major()], req)
		}
		if len(byMajor) <= 1 {
			continue
		}

		majors := make([]int64, 0, len(byMajor))
		for major := range byMajor {
			majors = append(majors, major)
		}
		sort.Slice(majors, func(i, j int) bool { return majors[i] < majors[j] })

		violation := Violation{Library: name}
		for _, major := range majors {
			violation.Majors = append(violation.Majors, MajorRequirements{Major: major, Requirements: byMajor[major]})
		}
		violations = append(violations, violation)
	}
	return violations, nil
}

// Validate returns a coherence error if any shared library is required at more
// than one major. The error names each diamond and the requirements on each
// side so the consumer can pin a single major.
func (c *Closure) Validate() error {
	violations, err := c.Violations()
	if err != nil {
		return err
	}
	if len(violations) == 0 {
		return nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "incoherent library closure: %d shared %s required at multiple majors",
		len(violations), plural(len(violations), "library", "libraries"))
	for _, violation := range violations {
		fmt.Fprintf(&b, "\n  %s:", violation.Library)
		for _, group := range violation.Majors {
			for _, req := range group.Requirements {
				fmt.Fprintf(&b, "\n    v%d (%s -> %s) required by %s",
					group.Major, req.Constraint, req.Resolved, req.RequiredBy)
			}
		}
	}
	return fmt.Errorf("%s", b.String())
}

func plural(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}
