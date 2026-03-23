package mcp

import (
	"context"
	"testing"
)

func TestSymbolToolsRegistered(t *testing.T) {
	ctx := context.Background()
	server, err := NewServer(ctx, "test-version")
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	tools := server.ListTools()
	symbolTools := []string{
		"symbols/references",
		"symbols/definition",
		"symbols/diagnostics",
		"symbols/hover",
		"symbols/impact",
	}

	for _, expected := range symbolTools {
		found := false
		for _, tool := range tools {
			if tool.Name == expected {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("symbol tool %s not registered", expected)
		}
	}
}

func TestParseLineCol(t *testing.T) {
	tests := []struct {
		name    string
		args    map[string]string
		wantL   int
		wantC   int
		wantErr bool
	}{
		{
			name:  "valid",
			args:  map[string]string{"line": "42", "column": "10"},
			wantL: 42, wantC: 10,
		},
		{
			name:    "invalid line",
			args:    map[string]string{"line": "abc", "column": "10"},
			wantErr: true,
		},
		{
			name:    "invalid column",
			args:    map[string]string{"line": "42", "column": "xyz"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l, c, err := parseLineCol(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if l != tt.wantL || c != tt.wantC {
				t.Fatalf("got (%d, %d), want (%d, %d)", l, c, tt.wantL, tt.wantC)
			}
		})
	}
}

func TestLocationToMap(t *testing.T) {
	m := locationToMap(nil)
	if len(m) != 0 {
		t.Fatalf("expected empty map for nil, got %v", m)
	}
}
