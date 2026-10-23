//go:build !windows

package containerd

import (
	"errors"
	"os"
	"os/exec"
	"syscall"

	core "github.com/openclaw/crabbox/internal/cli"
)

// forwardPublishingSupported reports whether this operator can own a detached
// SSH forward. A Unix operator has the process-group and advisory-lock
// contracts the owner relies on.
func forwardPublishingSupported() error { return nil }

// detachForwardOwner starts the helper in its own session so it outlives the
// one-shot `crabbox ports` process that spawned it.
func detachForwardOwner(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// tryForwardLock takes the owner lock without waiting. A held lock means a live
// owner; an acquired lock is returned as the release function and proves no
// owner is alive. create=false never creates state just to probe it.
func tryForwardLock(path string, create bool) (func(), bool, error) {
	flags := os.O_RDWR
	if create {
		flags |= os.O_CREATE
	}
	file, err := os.OpenFile(path, flags, 0o600)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, core.Exit(5, "open the forward owner lock %s: %v", path, err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, true, nil
		}
		return nil, false, core.Exit(5, "lock the forward owner file %s: %v", path, err)
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, false, nil
}

// lockForwardOwner takes the owner lock for the owner's whole lifetime.
func lockForwardOwner(path string) (func(), error) {
	release, held, err := tryForwardLock(path, true)
	if err != nil {
		return nil, err
	}
	if held {
		return nil, core.Exit(2, "another forward owner already holds %s", path)
	}
	return release, nil
}

// signalForwardOwnerProcess asks the owner to tear its forward down. The owner
// removes its record and exits; ESRCH means it is already gone.
func signalForwardOwnerProcess(pid int) error {
	if pid <= 0 {
		return core.Exit(2, "invalid forward owner pid %d", pid)
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}
