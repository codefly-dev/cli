package config

// ARCHITECTURE: Codefly owns the configuration file format, so Codefly — not a
// consumer repo's shell script — must own writing it. core/configurations reads
// `<scope-dir>/configurations/<profile>/<name>[.secret].env`; this file is the
// symmetric writer for exactly that layout.
//
// Everything here is deliberately conservative, because the alternative it
// replaces (hand-rolled `openssl rand > .env` scripts) got these wrong:
//   - a value is never overwritten unless --force, so re-running never rotates a
//     credential another service already holds;
//   - secret files are 0600 inside 0700 directories;
//   - a secret target that git does not ignore is refused, so a generated
//     credential cannot be committed by accident;
//   - plaintext is refused when a reference-only manifest owns the same logical
//     configuration, matching the load-time rule in core/configurations.
//
// Values are never returned to the command layer for printing. Callers get key
// names and presence only.

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// The configuration file layout core/configurations reads. Kept as constants so
// the writer and the reader cannot drift apart on a suffix.
const (
	// CommandName is this command group's name, used in error prefixes.
	CommandName = "config"
	// ConfigurationsDir is the directory each scope holds its profiles under.
	ConfigurationsDir = "configurations"
	// DefaultProfile is the configuration profile used when --env is absent.
	DefaultProfile = "local"

	// SecretSuffix marks a plaintext secret configuration.
	SecretSuffix = ".secret.env"
	// PlainSuffix marks a non-secret configuration.
	PlainSuffix = ".env"
	// referenceSuffix marks a reference-only manifest, which this package never
	// writes — it only refuses to shadow one.
	referenceSuffix = ".secret.ref.env"
)

// Target is one resolved configuration file: a scope directory (workspace root
// or service root), an environment configuration profile, a configuration name,
// and whether it holds secrets.
type Target struct {
	// ScopeDir is the workspace or service directory that owns
	// `configurations/`.
	ScopeDir string
	// Profile is the environment's configuration profile (e.g. "local").
	Profile string
	// Name is the configuration name — the file's basename before the suffix.
	Name string
	// Secret selects the `.secret.env` suffix and the stricter file mode.
	Secret bool
}

// Dir is the profile directory holding the target file.
func (t Target) Dir() string {
	return filepath.Join(t.ScopeDir, ConfigurationsDir, t.Profile)
}

// Path is the configuration file this target reads and writes.
func (t Target) Path() string {
	return filepath.Join(t.Dir(), t.Name+t.suffix())
}

func (t Target) suffix() string {
	if t.Secret {
		return SecretSuffix
	}
	return PlainSuffix
}

// referencePath is the reference-only manifest for the same logical
// configuration. core/configurations rejects a load that has both, so a write
// must too.
func (t Target) referencePath() string {
	return filepath.Join(t.Dir(), t.Name+referenceSuffix)
}

func (t Target) fileMode() os.FileMode {
	if t.Secret {
		return 0o600
	}
	return 0o644
}

// Display is the workspace-relative label used in output. It never contains a
// value.
func (t Target) Display(root string) string {
	if rel, err := filepath.Rel(root, t.Path()); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return t.Path()
}

// ErrNotIgnored is the sentinel for a secret target that git would track.
var ErrNotIgnored = errors.New("secret configuration file is not ignored by git")

// NotIgnoredError carries the offending path. It deliberately does NOT
// implement Unwrap: the CLI renders only an error chain's innermost layer, so
// unwrapping to the bare sentinel would drop the path and the fix from the
// message the user actually sees. Is() keeps errors.Is(err, ErrNotIgnored)
// working for callers.
type NotIgnoredError struct {
	Path string
}

func (e *NotIgnoredError) Error() string {
	return fmt.Sprintf("git does not ignore %s — add an ignore rule for it before storing a credential there", e.Path)
}

func (e *NotIgnoredError) Is(target error) bool { return target == ErrNotIgnored }

// Document is a parsed configuration file. Comments and blank lines are
// preserved by position so a codefly-owned write does not destroy a
// hand-maintained file's structure.
type Document struct {
	lines []string       // raw lines, with entry lines marked by index
	index map[string]int // key -> line index
}

func parseDocument(data []byte) *Document {
	doc := &Document{index: map[string]int{}}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		doc.lines = append(doc.lines, line)
		key, _, ok := splitEntry(line)
		if !ok {
			continue
		}
		// Last occurrence wins, matching how an env reader consumes the file.
		doc.index[key] = len(doc.lines) - 1
	}
	return doc
}

func splitEntry(line string) (string, string, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return "", "", false
	}
	key, value, ok := strings.Cut(trimmed, "=")
	key = strings.TrimSpace(key)
	if !ok || key == "" {
		return "", "", false
	}
	return key, value, true
}

// Has reports whether the key is present with a non-empty value.
func (d *Document) Has(key string) bool {
	i, ok := d.index[key]
	if !ok {
		return false
	}
	_, value, _ := splitEntry(d.lines[i])
	return strings.TrimSpace(value) != ""
}

// Keys returns the key names in file order. Values are never exposed.
func (d *Document) Keys() []string {
	keys := make([]string, 0, len(d.index))
	for key := range d.index {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return d.index[keys[i]] < d.index[keys[j]] })
	return keys
}

// Set writes key=value, replacing an existing line in place or appending.
func (d *Document) Set(key, value string) {
	line := key + "=" + value
	if i, ok := d.index[key]; ok {
		d.lines[i] = line
		return
	}
	d.lines = append(d.lines, line)
	d.index[key] = len(d.lines) - 1
}

func (d *Document) Bytes() []byte {
	if len(d.lines) == 0 {
		return nil
	}
	return []byte(strings.Join(d.lines, "\n") + "\n")
}

// Load reads the target's current Document. A missing file is an empty
// Document, not an error — writing is how it gets created.
func Load(target Target) (*Document, error) {
	data, err := os.ReadFile(target.Path())
	if errors.Is(err, os.ErrNotExist) {
		return parseDocument(nil), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", target.Path(), err)
	}
	return parseDocument(data), nil
}

// Write persists the Document, creating the profile directory and enforcing the
// target's file mode. Secret targets are refused when git would track them.
func Write(target Target, doc *Document) error {
	if err := guardReferenceManifest(target); err != nil {
		return err
	}
	dirMode := os.FileMode(0o755)
	if target.Secret {
		dirMode = 0o700
	}
	if err := os.MkdirAll(target.Dir(), dirMode); err != nil {
		return fmt.Errorf("create %s: %w", target.Dir(), err)
	}
	if target.Secret {
		ignored, err := gitIgnores(target.Path())
		if err != nil {
			return err
		}
		if !ignored {
			return &NotIgnoredError{Path: target.Path()}
		}
	}
	if err := os.WriteFile(target.Path(), doc.Bytes(), target.fileMode()); err != nil {
		return fmt.Errorf("write %s: %w", target.Path(), err)
	}
	// WriteFile only applies the mode on create; an existing file keeps its own.
	if err := os.Chmod(target.Path(), target.fileMode()); err != nil {
		return fmt.Errorf("set mode on %s: %w", target.Path(), err)
	}
	return nil
}

func guardReferenceManifest(target Target) error {
	if _, err := os.Stat(target.referencePath()); err == nil {
		return fmt.Errorf(
			"%s already owns configuration %q through a reference-only manifest; "+
				"resolve the value in its secret backend instead of writing plaintext",
			target.referencePath(), target.Name)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect %s: %w", target.referencePath(), err)
	}
	return nil
}

// gitIgnores reports whether git ignores path. A directory outside any git
// repository is not a tracking risk, so it reports true.
func gitIgnores(path string) (bool, error) {
	dir := filepath.Dir(path)
	cmd := exec.Command("git", "check-ignore", "-q", "--no-index", path)
	cmd.Dir = dir
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		switch exit.ExitCode() {
		case 1:
			return false, nil // tracked or untracked, but not ignored
		case 128:
			return true, nil // not a git repository
		}
	}
	if errors.Is(err, exec.ErrNotFound) {
		return true, nil // no git available; nothing to protect against
	}
	return false, fmt.Errorf("check whether git ignores %s: %w", path, err)
}

// Format selects the encoding of a generated secret.
type Format string

const (
	FormatHex    Format = "hex"
	FormatBase64 Format = "base64"
)

// ParseFormat validates a --format value.
func ParseFormat(raw string) (Format, error) {
	switch Format(strings.TrimSpace(strings.ToLower(raw))) {
	case FormatHex:
		return FormatHex, nil
	case FormatBase64:
		return FormatBase64, nil
	default:
		return "", fmt.Errorf("unknown --format %q: expected hex or base64", raw)
	}
}

// Generate returns a cryptographically random value of size bytes in the given
// encoding.
func Generate(format Format, size int) (string, error) {
	if size <= 0 {
		return "", fmt.Errorf("--bytes must be positive, got %d", size)
	}
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate %d random bytes: %w", size, err)
	}
	switch format {
	case FormatHex:
		return hex.EncodeToString(buf), nil
	case FormatBase64:
		return base64.StdEncoding.EncodeToString(buf), nil
	default:
		return "", fmt.Errorf("unknown format %q", format)
	}
}
