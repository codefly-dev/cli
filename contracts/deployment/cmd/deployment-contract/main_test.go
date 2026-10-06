package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
)

func TestExecutableProtocol(t *testing.T) {
	inventory, err := os.ReadFile("../../testdata/base.json")
	if err != nil {
		t.Fatal(err)
	}
	context, err := os.ReadFile("../../testdata/base-context.json")
	if err != nil {
		t.Fatal(err)
	}
	request := `{"inventory":` + string(inventory) + `,"context":` + string(context) + `}`
	for _, tc := range []struct {
		name, input, rule string
		code              int
	}{
		{"valid", request, "", 0},
		{"extra-field", `{"inventory":{},"context":{},"skip":true}`, "REQUEST_SHAPE", 1},
		{"missing-context", `{"inventory":{}}`, "REQUEST_SHAPE", 1},
		{"duplicate-envelope", `{"inventory":{},"inventory":{},"context":{}}`, "JSON_DUPLICATE", 1},
		{"unknown-dev-flag", strings.Replace(request, `"complete": true`, `"complete": true, "dev": true`, 1), "SCHEMA_UNKNOWN", 1},
		{"malformed", `{`, "JSON_SYNTAX", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, diagnostic bytes.Buffer
			code := run([]string{"validate"}, strings.NewReader(tc.input), &out, &diagnostic)
			if code != tc.code {
				t.Fatalf("exit %d: %s", code, out.String())
			}
			var r response
			if err := json.Unmarshal(out.Bytes(), &r); err != nil {
				t.Fatal(err)
			}
			if tc.code == 0 {
				if !r.Valid || r.Violation != nil || r.Canonical == nil || r.Digest == nil || len(r.Rows) != 1 {
					t.Fatalf("invalid success: %+v", r)
				}
			} else {
				if r.Valid || r.Canonical != nil || r.Digest != nil || r.Rows != nil || r.Violation == nil || r.Violation.Rule != tc.rule {
					t.Fatalf("refusal leaked usable output or wrong rule: %+v", r)
				}
			}
		})
	}
	for _, args := range [][]string{nil, {"validate", "--skip"}, {"validate", "--dev"}} {
		if run(args, strings.NewReader(request), io.Discard, io.Discard) != 2 {
			t.Fatal("unknown invocation accepted")
		}
	}
	for _, kind := range []string{"schema", "context-schema"} {
		var out bytes.Buffer
		if run([]string{kind}, nil, &out, io.Discard) != 0 || !json.Valid(out.Bytes()) {
			t.Fatal("invalid schema command")
		}
	}
}
