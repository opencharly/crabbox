//go:build windows

package containerd

import (
	"os/exec"

	core "github.com/openclaw/crabbox/internal/cli"
)

// forwardPublishingSupported refuses published forwards on Windows: the owner
// depends on a session-detached process plus an advisory lock that the platform
// files do not implement here. Listing still reports nerdctl's mappings.
func forwardPublishingSupported() error {
	return core.Exit(2, "containerd published forwards need a Unix operator; run `crabbox ports --publish` from Linux or macOS")
}

func detachForwardOwner(_ *exec.Cmd) {}

func lockForwardOwner(_ string) (func(), error) {
	return nil, forwardPublishingSupported()
}

func forwardOwnerLockHeld(_ string) (bool, error) {
	return false, forwardPublishingSupported()
}

func signalForwardOwnerProcess(_ int) error {
	return forwardPublishingSupported()
}
