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

// lockForwardOwner takes the owner lock. The returned release must be held for
// the owner's whole lifetime; the kernel releases it if the owner dies, which is
// exactly the liveness signal --unpublish reads.
func lockForwardOwner(path string) (func(), error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, core.Exit(5, "open the forward owner lock %s: %v", path, err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, core.Exit(2, "another forward owner already holds %s", path)
		}
		return nil, core.Exit(5, "lock the forward owner file %s: %v", path, err)
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}

// forwardOwnerLockHeld reports whether a live owner still holds the lock. A
// free lock proves the owner is gone, so a recorded pid is never signalled on
// the strength of the pid alone.
func forwardOwnerLockHeld(path string) (bool, error) {
	file, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, core.Exit(5, "open the forward owner lock %s: %v", path, err)
	}
	defer file.Close()
	err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		return false, nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return true, nil
	}
	return false, core.Exit(5, "probe the forward owner lock %s: %v", path, err)
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
