package get

import "testing"

func TestEndpointsCommandReturnsErrorsThroughCobra(t *testing.T) {
	if EndpointsCmd.RunE == nil || EndpointsCmd.Run != nil {
		t.Fatal("endpoints command is not exclusively RunE")
	}
	if err := EndpointsCmd.Args(EndpointsCmd, []string{"one", "two"}); err == nil {
		t.Fatal("endpoints command accepted two service names")
	}
}
