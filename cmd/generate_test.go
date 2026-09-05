package cmd

import "testing"

func TestGenerateSubcommandsResolveLowercaseAndAliases(t *testing.T) {
	cases := map[string]string{
		"grpc":    "grpc",
		"gRPC":    "grpc",
		"openapi": "openapi",
		"openAPI": "openapi",
		"swagger": "openapi",
	}
	for input, want := range cases {
		found, _, err := GenerateCmd.Find([]string{input})
		if err != nil {
			t.Errorf("generate %s: %v", input, err)
			continue
		}
		if found.Name() != want {
			t.Errorf("generate %s resolved to %q, want %q", input, found.Name(), want)
		}
	}
}
