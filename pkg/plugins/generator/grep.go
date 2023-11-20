package generator

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/codefly-dev/core/shared"
)

type Grep struct {
	Root       string
	Extensions []string
	Matcher    Matcher
	Excludes   []string
}

type Matcher interface {
	Match(data []byte) [][]byte
}

type RegexpMatcher struct {
	Pattern *regexp.Regexp
}

func (r RegexpMatcher) Match(data []byte) [][]byte {
	return r.Pattern.FindAll(data, -1)
}

func NewRegexpMatcher(pattern string) (Matcher, error) {
	logger := shared.NewLogger("generator.NewRegexpMatcher")
	if pattern == "" {
		return nil, logger.Errorf("pattern cannot be empty")
	}
	reg, err := regexp.Compile(pattern)
	if err != nil {
		return nil, logger.Wrapf(err, "cannot compile pattern")
	}
	return &RegexpMatcher{Pattern: reg}, nil
}

func NewGrep(root string, extensions []string, matcher Matcher, excludes []string) *Grep {
	return &Grep{Root: root, Extensions: extensions, Matcher: matcher, Excludes: excludes}
}

type Match struct {
	Matches []string
}

type MatchSummary struct {
	FileMatches map[string]Match
	Hits        []string
}

func (summary *MatchSummary) Pretty() string {
	var ss []string
	for file, match := range summary.FileMatches {
		ss = append(ss, fmt.Sprintf("%s: %v", path.Base(file), match.Matches))
	}
	return strings.Join(ss, "\n")
}

func (g *Grep) Exclude(p string) bool {
	for _, excl := range g.Excludes {
		if strings.Contains(p, excl) {
			return true
		}
	}
	return false
}

type HitExpansion = func(hit string) []string

func (g *Grep) FindFiles(expand HitExpansion) (*MatchSummary, error) {
	logger := shared.NewLogger("generator.Grep.FindFiles")
	fileMatches := make(map[string]Match)
	hits := make(map[string]bool)
	expandedHits := make(map[string]bool)

	err := filepath.Walk(g.Root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Skip directories
		if info.IsDir() {
			return nil
		}
		if !slices.Contains(g.Extensions, filepath.Ext(path)) {
			return nil
		}

		if g.Exclude(path) {
			return nil
		}

		// Read the file
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		// Search for the pattern
		matches := g.Matcher.Match(data)
		if len(matches) > 0 {
			m := Match{Matches: make([]string, 0, len(matches))}
			matchesForFile := make(map[string]bool)
			for _, match := range matches {
				hit := string(match)
				if !matchesForFile[hit] {
					m.Matches = append(m.Matches, hit)
					matchesForFile[hit] = true
				}
				if !hits[hit] {
					hits[hit] = true
					if expand != nil {
						for _, other := range expand(hit) {
							expandedHits[other] = true
						}
					}
				}
			}
			fileMatches[path] = m
		}
		return nil
	})
	for other := range expandedHits {
		otherMatcher, err := NewRegexpMatcher(other)
		if err != nil {
			return nil, logger.Wrapf(err, "cannot create new matcher from hit expansion")
		}
		otherGrep := NewGrep(g.Root, g.Extensions, otherMatcher, g.Excludes)
		otherResult, err := otherGrep.FindFiles(nil)
		if err != nil {
			return nil, logger.Wrapf(err, "cannot find files from hit expansion")
		}

		for file, match := range otherResult.FileMatches {
			existingMatches := fileMatches[file].Matches
			existingMatches = append(existingMatches, match.Matches...)
			fileMatches[file] = Match{Matches: existingMatches}
		}

		for _, hit := range otherResult.Hits {
			hits[hit] = true
		}
	}
	if err != nil {
		return nil, logger.Wrapf(err, "cannot walk through files")
	}
	var uniqueHits []string
	for hit := range hits {
		uniqueHits = append(uniqueHits, hit)
	}
	return &MatchSummary{FileMatches: fileMatches, Hits: uniqueHits}, nil
}
