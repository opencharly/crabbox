package cli

import "testing"

// The provider/helper channel is the only thing that turns a `ports` process
// into a forward owner; an operator environment must never trigger it.
func TestPortsForwardOwnerFromEnv(t *testing.T) {
	t.Setenv(PortsForwardOwnerSpecEnv, "")
	if spec := portsForwardOwnerFromEnv(); spec != "" {
		t.Fatalf("an empty environment produced owner spec=%q", spec)
	}
	t.Setenv(PortsForwardOwnerSpecEnv, " 41000:3000 ")
	if spec := portsForwardOwnerFromEnv(); spec != "41000:3000" {
		t.Fatalf("got spec=%q", spec)
	}
	t.Setenv("CRABBOX_INTERNAL_PORTS_FORWARD_PORT", "41000")
	if spec := portsForwardOwnerFromEnv(); spec != "41000:3000" {
		t.Fatalf("a stray legacy port variable changed the spec: %q", spec)
	}
}
