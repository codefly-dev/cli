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
