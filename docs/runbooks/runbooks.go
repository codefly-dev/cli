// Package runbooks embeds the how-to runbooks in this directory so the CLI
// (as `codefly help <topic>` help topics) and the MCP server (as the `how_to`
// tool) can serve them offline. The Markdown files remain the canonical,
// human-readable source; this package is only a typed accessor over them.
//
// Assets are co-located with the package per the repo's embed convention
// (see cmd/common/logo.go, pkg/cliupdate/service.go). Keeping the files here
// avoids a parent-directory //go:embed (which Go forbids) and keeps a single
// source of truth for both humans and agents.
package runbooks

import (
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

//go:embed *.md
var files embed.FS

// indexFile is the human-facing index; it is not a runbook topic.
const indexFile = "README.md"

// Runbook is a single how-to document.
type Runbook struct {
	// Name is the topic name: the file name without its .md extension
	// (e.g. "bump-go-version").
	Name string
	// Title is the first Markdown H1, with any "Runbook: " prefix removed.
	Title string
	// Summary is the first prose paragraph after the title.
	Summary string
	// Content is the full Markdown document.
	Content string
}

// Names returns the available runbook topic names, sorted, excluding the index.
func Names() []string {
	entries, err := fs.ReadDir(files, ".")
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || e.Name() == indexFile || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		names = append(names, strings.TrimSuffix(e.Name(), ".md"))
	}
	sort.Strings(names)
	return names
}

// List returns every runbook (name, title, summary; no Content), sorted by
// name, excluding the index. Use Get to fetch a runbook's full Content.
func List() []Runbook {
	names := Names()
	out := make([]Runbook, 0, len(names))
	for _, name := range names {
		r, err := Get(name)
		if err != nil {
			continue
		}
		r.Content = ""
		out = append(out, r)
	}
	return out
}

// Get returns the runbook for the given topic name. The name may be given with
// or without a trailing ".md". It returns an error for unknown topics.
func Get(name string) (Runbook, error) {
	name = strings.TrimSuffix(strings.TrimSpace(name), ".md")
	if name == "" {
		return Runbook{}, fmt.Errorf("empty runbook name")
	}
	data, err := files.ReadFile(name + ".md")
	if err != nil {
		return Runbook{}, fmt.Errorf("unknown runbook %q", name)
	}
	content := string(data)
	title, summary := titleAndSummary(content)
	return Runbook{Name: name, Title: title, Summary: summary, Content: content}, nil
}

// titleAndSummary extracts the first H1 and the first prose paragraph following
// it. Both are best-effort and degrade to empty strings.
func titleAndSummary(content string) (title, summary string) {
	lines := strings.Split(content, "\n")
	i := 0
	for ; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "# ") {
			title = strings.TrimSpace(strings.TrimPrefix(lines[i], "# "))
			title = strings.TrimPrefix(title, "Runbook: ")
			i++
			break
		}
	}
	for ; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ">") {
			if summary != "" {
				break
			}
			continue
		}
		summary = strings.TrimSpace(summary + " " + line)
	}
	return title, firstSentence(summary)
}

// firstSentence trims a paragraph to its first sentence so it reads well as a
// one-line command summary. It falls back to a length-capped prefix.
func firstSentence(s string) string {
	if i := strings.Index(s, ". "); i > 0 {
		return s[:i+1]
	}
	const max = 160
	if len(s) <= max {
		return s
	}
	if cut := strings.LastIndex(s[:max], " "); cut > 0 {
		return s[:cut] + "…"
	}
	return s[:max] + "…"
}
