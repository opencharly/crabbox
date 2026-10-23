package cli

import "testing"

// The provider/helper channel is the only thing that turns a `ports` process
// into a forward owner; an operator environment must never trigger it.
func TestPortsForwardOwnerFromEnv(t *testing.T) {
	t.Setenv(PortsForwardOwnerSpecEnv, "")
	t.Setenv(PortsForwardOwnerPortEnv, "")
	if spec, port := portsForwardOwnerFromEnv(); spec != "" || port != "" {
		t.Fatalf("an empty environment produced owner spec=%q port=%q", spec, port)
	}
	t.Setenv(PortsForwardOwnerSpecEnv, " 41000:3000 ")
	t.Setenv(PortsForwardOwnerPortEnv, " 41000 ")
	if spec, port := portsForwardOwnerFromEnv(); spec != "41000:3000" || port != "41000" {
		t.Fatalf("got spec=%q port=%q", spec, port)
	}
}
