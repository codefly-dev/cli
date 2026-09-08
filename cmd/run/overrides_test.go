package run

import (
	"reflect"
	"testing"
)

func TestParseSetOverrides(t *testing.T) {
	tests := []struct {
		name    string
		entries []string
		want    map[string]map[string]string
		wantErr bool
	}{
		{
			name:    "empty",
			entries: nil,
			want:    nil,
		},
		{
			name:    "single",
			entries: []string{"warden:CODEFLY__FIXTURE=dogfood"},
			want: map[string]map[string]string{
				"warden": {"CODEFLY__FIXTURE": "dogfood"},
			},
		},
		{
			name:    "multiple services and keys",
			entries: []string{"warden:A=1", "warden:B=2", "api:C=3"},
			want: map[string]map[string]string{
				"warden": {"A": "1", "B": "2"},
				"api":    {"C": "3"},
			},
		},
		{
			name:    "value may contain = and :",
			entries: []string{"warden:DSN=postgres://u:p@h:5432/db?x=y"},
			want: map[string]map[string]string{
				"warden": {"DSN": "postgres://u:p@h:5432/db?x=y"},
			},
		},
		{
			name:    "missing colon",
			entries: []string{"wardenKEY=val"},
			wantErr: true,
		},
		{
			name:    "missing equals",
			entries: []string{"warden:KEY"},
			wantErr: true,
		},
		{
			name:    "empty service",
			entries: []string{":KEY=val"},
			wantErr: true,
		},
		{
			name:    "empty key",
			entries: []string{"warden:=val"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSetOverrides(tt.entries)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseSetOverrides() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseSetOverrides() = %v, want %v", got, tt.want)
			}
		})
	}
}

// The solution-derived injection and an operator's --set can name the same key
// on the same service. --set must win because it is layered last, not because
// parseSetOverrides happens to let the final duplicate entry overwrite the
// earlier one — reorder that loop and the operator would silently lose.
func TestMergeOverridesLetsSetWinOverDerived(t *testing.T) {
	derived := map[string]map[string]string{
		"wiki/backend": {"CODEFLY__API_CONSUMES": "derived", "CODEFLY__KEPT": "yes"},
	}
	set := map[string]map[string]string{
		"wiki/backend": {"CODEFLY__API_CONSUMES": "pinned-by-hand"},
		"warden":       {"CODEFLY__FIXTURE": "dogfood"},
	}

	got := mergeOverrides(derived, set)
	want := map[string]map[string]string{
		"wiki/backend": {"CODEFLY__API_CONSUMES": "pinned-by-hand", "CODEFLY__KEPT": "yes"},
		"warden":       {"CODEFLY__FIXTURE": "dogfood"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mergeOverrides() = %v, want %v", got, want)
	}
}

// A flow with no overrides must be indistinguishable from one that never had
// any: parseSetOverrides returns nil for no entries, so merging nothing must
// too rather than handing the flow an empty non-nil map.
func TestMergeOverridesEmptyIsNil(t *testing.T) {
	if got := mergeOverrides(nil, nil); got != nil {
		t.Fatalf("mergeOverrides(nil, nil) = %v, want nil", got)
	}
}

// mergeOverrides must not write through into its inputs: derivedOverrides is a
// package var reassigned per run, and a merge that aliased it would let one
// run's --set leak into the next.
func TestMergeOverridesDoesNotMutateInputs(t *testing.T) {
	derived := map[string]map[string]string{"wiki/backend": {"K": "derived"}}
	mergeOverrides(derived, map[string]map[string]string{"wiki/backend": {"K": "set"}})
	if derived["wiki/backend"]["K"] != "derived" {
		t.Fatalf("mergeOverrides mutated its input: %v", derived)
	}
}
