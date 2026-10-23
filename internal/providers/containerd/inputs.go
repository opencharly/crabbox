package containerd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

// inputs are written into one directory and mounted read-only at /crabbox.
// The directory is private (0700), every file is 0600, and each file is
// written to a temporary name and renamed, because virtiofs tears in-place
// rewrites for roughly five seconds on a krun guest.
type inputs struct {
	dir    string
	remote bool
}

func (b *backend) prepareInputs(ctx context.Context, leaseID, publicKey string) (inputs, error) {
	dir, err := b.inputsDir(ctx, leaseID)
	if err != nil {
		return inputs{}, err
	}
	files := []struct {
		name    string
		content string
	}{
		{"authorized_keys", strings.TrimSpace(publicKey) + "\n"},
		{"lease.env", b.leaseEnv(leaseID)},
		{"bootstrap.sh", bootstrapScript()},
	}
	if b.runner().remote() {
		if err := b.writeRemoteInputs(ctx, dir, files); err != nil {
			return inputs{}, err
		}
		return inputs{dir: dir, remote: true}, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return inputs{}, core.Exit(2, "create containerd inputs directory %s: %v", dir, err)
	}
	for _, file := range files {
		if err := writeFileAtomic(filepath.Join(dir, file.name), file.content); err != nil {
			return inputs{}, core.Exit(2, "write containerd input %s: %v", file.name, err)
		}
	}
	return inputs{dir: dir}, nil
}

func (b *backend) inputsDir(ctx context.Context, leaseID string) (string, error) {
	if !b.runner().remote() {
		root, err := localStateRoot()
		if err != nil {
			return "", err
		}
		return filepath.Join(root, "crabbox", "containerd", "leases", leaseID, "inputs"), nil
	}
	result, err := b.runner().runShell(ctx, `printf '%s' "$HOME"`, nil, nil)
	if err != nil {
		return "", core.Exit(2, "resolve the remote home directory for containerd inputs: %v", err)
	}
	home := strings.TrimSpace(result.Stdout)
	if !strings.HasPrefix(home, "/") {
		return "", core.Exit(2, "remote home directory %q is not absolute; set a login shell that reports an absolute HOME", home)
	}
	return filepath.Join(home, ".local", "state", "crabbox", "containerd", "leases", leaseID, "inputs"), nil
}

func (b *backend) leaseEnv(leaseID string) string {
	c := b.cfg.Containerd
	lines := []string{
		"CRABBOX_LEASE_ID=" + leaseID,
		"CRABBOX_SSH_USER=" + c.User,
		"CRABBOX_WORK_ROOT=" + c.WorkRoot,
		"CRABBOX_SSH_PORT=" + defaultSSHPort,
		"CRABBOX_PROVIDER=containerd",
		"CRABBOX_NAMESPACE=" + c.Namespace,
		"CRABBOX_NODE_ADDRESS=" + c.NodeAddress,
	}
	return strings.Join(lines, "\n") + "\n"
}

func (b *backend) writeRemoteInputs(ctx context.Context, dir string, files []struct{ name, content string }) error {
	if result, err := b.runner().runShell(ctx, "umask 077; mkdir -p "+core.ShellQuote(dir)+" && chmod 0700 "+core.ShellQuote(dir), nil, nil); err != nil {
		return core.Exit(2, "create remote containerd inputs directory %s: %v", dir, err)
	} else if result.ExitCode != 0 {
		return core.Exit(2, "create remote containerd inputs directory %s: %s", dir, strings.TrimSpace(result.Stderr))
	}
	for _, file := range files {
		target := dir + "/" + file.name
		tmp := target + ".tmp." + randomSuffix()
		script := "umask 077; cat > " + core.ShellQuote(tmp) + " && chmod 0600 " + core.ShellQuote(tmp) + " && mv -f " + core.ShellQuote(tmp) + " " + core.ShellQuote(target)
		result, err := b.runner().runShellInput(ctx, script, file.content)
		if err != nil || result.ExitCode != 0 {
			return core.Exit(2, "write remote containerd input %s: %v %s", file.name, err, strings.TrimSpace(result.Stderr))
		}
	}
	return nil
}

func (b *backend) removeInputs(ctx context.Context, dir string) error {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	parent := filepath.Dir(dir)
	if b.runner().remote() {
		script := "rm -rf " + core.ShellQuote(dir) + "; rmdir " + core.ShellQuote(parent) + " 2>/dev/null || true"
		result, err := b.runner().runShell(ctx, script, nil, nil)
		if err != nil {
			return err
		}
		if result.ExitCode != 0 {
			return core.Exit(2, "remove remote containerd inputs %s: %s", dir, strings.TrimSpace(result.Stderr))
		}
		return nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	// The lease directory holds only the inputs mount; remove it when empty so a
	// released lease leaves no empty directory behind.
	if parent != dir {
		if entries, err := os.ReadDir(parent); err == nil && len(entries) == 0 {
			_ = os.Remove(parent)
		}
	}
	return nil
}

func writeFileAtomic(path, content string) error {
	tmp := fmt.Sprintf("%s.tmp.%s", path, randomSuffix())
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func randomSuffix() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "00000000"
	}
	return hex.EncodeToString(buf[:])
}

func localStateRoot() (string, error) {
	if root := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); root != "" {
		return root, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", core.Exit(2, "resolve the local state directory for containerd inputs: %v", err)
	}
	return filepath.Join(home, ".local", "state"), nil
}
