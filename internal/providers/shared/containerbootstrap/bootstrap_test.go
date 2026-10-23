package containerbootstrap

import (
	"strings"
	"testing"
)

func TestScriptOwnsTheSSHContract(t *testing.T) {
	for _, want := range []string{
		"CRABBOX_AUTHORIZED_KEY",
		"/crabbox/authorized_keys",
		"CRABBOX_SSH_USER",
		"CRABBOX_WORK_ROOT",
		"CRABBOX_SSH_PORT",
		"exec /usr/sbin/sshd -D -e -p \"$ssh_port\"",
		Tools,
		Packages,
	} {
		if !strings.Contains(Script, want) {
			t.Fatalf("bootstrap script missing %q", want)
		}
	}
	if strings.Contains(Script, "docker exec") || strings.Contains(Script, "nerdctl exec") {
		t.Fatalf("bootstrap script must not exec into a container")
	}
}

func TestOneScriptForEveryProvider(t *testing.T) {
	// The constants are the single source the local-container aliases point at;
	// a second literal copy would let the guest contract drift.
	if Script == "" || Tools == "" || Packages == "" {
		t.Fatal("shared bootstrap constants must not be empty")
	}
	if !strings.Contains(Script, Tools) || !strings.Contains(Script, Packages) {
		t.Fatal("Script must be built from the shared Tools/Packages constants")
	}
}
