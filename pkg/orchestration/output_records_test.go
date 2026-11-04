package orchestration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
)

// variable is one record offered to the writer.
func variable(key string, value any) *resources.EnvironmentVariable {
	return &resources.EnvironmentVariable{Key: key, Value: value}
}

// TestTheOutputWriterRefusesRecordsTheReaderCannotParse is the counterexample
// the whole-record rule exists for: an unrelated, UNBOUND value whose text
// carries an LF and a second `KEY=value` line. Semantic carrier matching
// cannot see it — the record's own key is nothing special — but the reader
// scans lines, so that tail lands in the process environment as a value for a
// key the run never offered. Refusing every unrepresentable record, bound or
// not, is what closes it.
func TestTheOutputWriterRefusesRecordsTheReaderCannotParse(t *testing.T) {
	ctx := context.Background()
	for _, probe := range []struct {
		name    string
		records []*resources.EnvironmentVariable
	}{
		{"a line break smuggles a second record", []*resources.EnvironmentVariable{
			variable("CODEFLY__NOTE", "note\nCODEFLY__WORKSPACE_CONFIGURATION__DOCS__README=substituted"),
		}},
		{"a carriage return the reader would silently eat", []*resources.EnvironmentVariable{
			variable("CODEFLY__NOTE", "note\r"),
		}},
		{"a NUL a process environment cannot carry", []*resources.EnvironmentVariable{
			variable("CODEFLY__NOTE", "note\x00tail"),
		}},
		{"a key outside the reader's prefix", []*resources.EnvironmentVariable{
			variable("NOTE", "plain"),
		}},
		{"a key with no suffix at all", []*resources.EnvironmentVariable{
			variable("CODEFLY__", "plain"),
		}},
		{"a lowercase key", []*resources.EnvironmentVariable{
			variable("CODEFLY__note", "plain"),
		}},
		{"a hyphenated key", []*resources.EnvironmentVariable{
			variable("CODEFLY__READ-WRITE", "plain"),
		}},
		{"a key carrying the separator", []*resources.EnvironmentVariable{
			variable("CODEFLY__NOTE=X", "plain"),
		}},
		{"a key carrying a line break", []*resources.EnvironmentVariable{
			variable("CODEFLY__NOTE\nCODEFLY__OTHER", "plain"),
		}},
		// The offending record is LAST, which is the point: a check that ran
		// per append would already have written the valid one.
		{"an invalid record after a valid one", []*resources.EnvironmentVariable{
			variable("CODEFLY__WORKSPACE_CONFIGURATION__DOCS__README", "fine"),
			variable("CODEFLY__NOTE", "note\nCODEFLY__WORKSPACE_CONFIGURATION__DOCS__README=substituted"),
		}},
	} {
		t.Run(probe.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "runtime.env")
			require.Error(t, appendEnvironmentVariablesToFile(ctx, path, probe.records))
			_, err := os.Stat(path)
			require.True(t, os.IsNotExist(err), "an absent output stays absent")

			// And an existing artifact keeps its exact bytes.
			existing := filepath.Join(t.TempDir(), "runtime.env")
			require.NoError(t, appendEnvironmentVariablesToFile(ctx, existing, []*resources.EnvironmentVariable{
				variable("CODEFLY__SENTINEL", "untouched"),
			}))
			before, err := os.ReadFile(existing)
			require.NoError(t, err)
			require.Error(t, appendEnvironmentVariablesToFile(ctx, existing, probe.records))
			after, err := os.ReadFile(existing)
			require.NoError(t, err)
			require.Equal(t, before, after, "a refused batch leaves the existing artifact byte-identical")
		})
	}
}

// TestRefusalDiagnosticsCarryNoValuesOrMalformedKeys keeps the refusal from
// becoming the leak: a malformed key and every value are the halves an
// attacker controls, so the message names a record index or an already valid
// carrier and a generic reason.
func TestRefusalDiagnosticsCarryNoValuesOrMalformedKeys(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "runtime.env")

	err := appendEnvironmentVariablesToFile(ctx, path, []*resources.EnvironmentVariable{
		variable("CODEFLY__FINE", "ok"),
		variable("totally-made-up-key", "secret-value"),
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "record 1")
	require.NotContains(t, err.Error(), "totally-made-up-key")
	require.NotContains(t, err.Error(), "secret-value")

	err = appendEnvironmentVariablesToFile(ctx, path, []*resources.EnvironmentVariable{
		variable("CODEFLY__TOKEN", "line\nsecret-value"),
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "CODEFLY__TOKEN")
	require.NotContains(t, err.Error(), "secret-value")
}

// readerGrammar is sdk-go's validRuntimeEnvironmentName as of
// codefly-dev/sdk-go#95, transcribed for this test ONLY: CODEFLY__ plus
// [A-Z0-9_], or a configuration document's carrier — a document prefix and
// exactly 64 lowercase hex digits.
//
// The CLI does not depend on root sdk-go and this must not make it: the
// production check lives in validOutputKey, and the agreement between the two
// is itself asserted below, so a divergence shows up here rather than in a
// run. A roundtrip through the real reader is a separate, out-of-process
// check against a built binary.
func readerGrammar(name string) bool {
	for _, prefix := range []string{"CODEFLY__CONFIGURATION_DOCUMENT_V1__", "CODEFLY__SECRET_CONFIGURATION_DOCUMENT_V1__"} {
		if digest, found := strings.CutPrefix(name, prefix); found && len(digest) == 64 && strings.Trim(digest, "0123456789abcdef") == "" {
			return true
		}
	}
	if !strings.HasPrefix(name, "CODEFLY__") || len(name) == len("CODEFLY__") {
		return false
	}
	for _, character := range name {
		switch {
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case character == '_':
		default:
			return false
		}
	}
	return true
}

// TestTheWriterAgreesWithTheReadersGrammar is what keeps the production rule
// honest: every name the writer admits the reader must admit, and the other
// way round.
func TestTheWriterAgreesWithTheReadersGrammar(t *testing.T) {
	for _, name := range []string{
		"CODEFLY__WORKSPACE_CONFIGURATION__DOCS__README", "CODEFLY__A", "CODEFLY__9", "CODEFLY___",
		"CODEFLY__", "CODEFLY", "codefly__A", "CODEFLY__a", "CODEFLY__A-B", "CODEFLY__A B",
		"CODEFLY__A=B", "CODEFLY__A\nB", "CODEFLY__Ä", "", "NOTE",
		resources.ConfigurationDocumentKey(resources.ConfigurationWorkspace, "fixture-composition", "local", false),
		resources.ConfigurationDocumentKey("saas/accounts", "fixture-composition", "local", true),
		// The document shape admits its own digest and nothing near it.
		resources.ConfigurationDocumentPrefix + strings.Repeat("a", 63),
		resources.ConfigurationDocumentPrefix + strings.Repeat("a", 65),
		resources.ConfigurationDocumentPrefix + strings.Repeat("g", 64),
		resources.ConfigurationDocumentPrefix + strings.Repeat("a", 63) + "-",
		resources.ConfigurationDocumentPrefix + strings.Repeat("A", 64),
		resources.ConfigurationDocumentPrefix,
		"CODEFLY__CONFIGURATION_DOCUMENT_V2__" + strings.Repeat("a", 64),
		"CODEFLY__WORKSPACE_CONFIGURATION__" + strings.Repeat("a", 64),
	} {
		require.Equal(t, readerGrammar(name), validOutputKey(name) == nil,
			"the writer and the supported reader disagree about a carrier name")
	}
}

// TestRepresentableRecordsRoundTrip is the positive half: an empty value and
// a value holding several `=` survive unchanged, because the reader cuts at
// the FIRST separator only.
func TestRepresentableRecordsRoundTrip(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "runtime.env")
	require.NoError(t, appendEnvironmentVariablesToFile(ctx, path, []*resources.EnvironmentVariable{
		variable("CODEFLY__WORKSPACE_CONFIGURATION__DOCS__README", "k=v=w"),
		variable("CODEFLY__EMPTY", ""),
		variable("CODEFLY__DIGITS_9", "9"),
	}))
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	values := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(string(body), "\n"), "\n") {
		name, value, found := strings.Cut(strings.TrimSuffix(line, "\r"), "=")
		require.True(t, found)
		require.True(t, readerGrammar(name))
		values[name] = value
	}
	require.Equal(t, map[string]string{
		"CODEFLY__WORKSPACE_CONFIGURATION__DOCS__README": "k=v=w",
		"CODEFLY__EMPTY":    "",
		"CODEFLY__DIGITS_9": "9",
	}, values)
}

// TestTheWriterRefusesMalformedRetainedRecords covers the rewrite direction.
// The writer rereads the file and rewrites it whole, so a malformed line
// already there would be reproduced under this run's name. Syntax says
// nothing about where a well formed old record came from — this is integrity,
// not provenance — but laundering a record the reader refuses is still the
// writer's doing.
func TestTheWriterRefusesMalformedRetainedRecords(t *testing.T) {
	ctx := context.Background()
	for _, retained := range []string{
		"NOTE=plain\n",
		"CODEFLY__lower=plain\n",
		"no-separator-at-all\n",
	} {
		path := filepath.Join(t.TempDir(), "runtime.env")
		require.NoError(t, os.WriteFile(path, []byte(retained), 0o600))
		require.Error(t, appendEnvironmentVariablesToFile(ctx, path, []*resources.EnvironmentVariable{
			variable("CODEFLY__FINE", "ok"),
		}))
		body, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, retained, string(body), "the refused rewrite left the file alone")
	}
}

// TestAStructuredConfigurationDocumentIsExported is the defect cli#1213's
// check surfaced, closed: resources.ConfigurationDocumentKey is a prefix plus
// a LOWERCASE hex SHA-256, and a run that delivers a structured configuration
// document to the --output-env service — platform-core's merged
// fixture-composition, which saas/auth-gateway receives — refused the whole
// artifact ("its name is not uppercase ASCII…"). The carrier is now written
// under the name core mints, and a consumer that looks it up the way the SDK
// does (that same function, exactly) recovers the document.
func TestAStructuredConfigurationDocumentIsExported(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "runtime.env")
	native := resources.NewRuntimeContextNative()
	document := &basev0.Configuration{Origin: resources.ConfigurationWorkspace, RuntimeContext: native, Infos: []*basev0.ConfigurationInformation{{
		Name: "fixture-composition",
		Data: &basev0.ConfigurationData{Kind: "yaml", Content: []byte("schema: codefly.dev/authority/fixture-composition/v1\nroles: []\n")},
	}}}
	key := resources.ConfigurationDocumentKey(resources.ConfigurationWorkspace, "fixture-composition", "local", false)
	require.NoError(t, validOutputKey(key))

	require.NoError(t, AppendServiceProcessConfigurationsToFile(ctx, path, localRunEnvironment(), native, nil,
		[]*basev0.Configuration{document}, nil, nil))
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	values := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(string(body), "\n"), "\n") {
		name, value, found := strings.Cut(line, "=")
		require.True(t, found)
		require.True(t, readerGrammar(name), "the supported reader admits every record written")
		values[name] = value
	}
	encoded, found := values[key]
	require.True(t, found, "the document rides the carrier core names")
	content, err := resources.DecodeConfigurationDocument(encoded, resources.ConfigurationWorkspace, "fixture-composition", "local", false)
	require.NoError(t, err)
	require.JSONEq(t, `{"schema":"codefly.dev/authority/fixture-composition/v1","roles":[]}`, string(content))
}
