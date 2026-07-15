package generate

import (
	"testing"
)

func TestGenerateCommandsReturnErrorsThroughCobra(t *testing.T) {
	for name, command := range map[string]struct {
		runE bool
		args bool
	}{
		"proto":   {runE: ProtoCmd.RunE != nil, args: ProtoCmd.Args != nil},
		"grpc":    {runE: GRPCCmd.RunE != nil, args: GRPCCmd.Args != nil},
		"openapi": {runE: OpenAPICmd.RunE != nil, args: OpenAPICmd.Args != nil},
	} {
		t.Run(name, func(t *testing.T) {
			if !command.runE {
				t.Fatal("command has no RunE handler")
			}
			if !command.args {
				t.Fatal("command has no argument validator")
			}
		})
	}
}
