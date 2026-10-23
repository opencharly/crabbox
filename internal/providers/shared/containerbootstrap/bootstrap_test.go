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

// A browser that answers --version but cannot render is not a working browser:
// install and the post-install check both go through the headless probe.
func TestBrowserCheckProvesAHeadlessRender(t *testing.T) {
	for _, want := range []string{
		"browser_can_run_headless",
		"browser_can_run_headless \"$candidate_path\"",
		"--headless --disable-gpu --disable-dev-shm-usage --no-sandbox --dump-dom about:blank",
		"--headless --screenshot",
	} {
		if !strings.Contains(BrowserInstallScript, want) {
			t.Fatalf("browser install script missing the headless probe %q", want)
		}
	}
	if strings.Contains(BrowserInstallScript, `if candidate_path="$(command -v "$candidate" 2>/dev/null)" && "$candidate_path" --version >/dev/null 2>&1; then`) {
		t.Fatal("browser selection still accepts a version-only check")
	}
	if !strings.Contains(Script, BrowserInstallScript) {
		t.Fatal("the bootstrap script must include the browser install script")
	}
}

// The root bootstrap probe needs --no-sandbox, but the lease browser wrapper
// must keep its sandbox: exactly one occurrence, and none after the wrapper
// definition.
func TestHeadlessSandboxRelaxationStaysInTheProbe(t *testing.T) {
	if count := strings.Count(Script, "--no-sandbox"); count != 1 {
		t.Fatalf("--no-sandbox occurs %d times; it must be confined to the bootstrap probe", count)
	}
	wrapper := strings.Index(Script, "browser_wrapper=/usr/local/bin/crabbox-browser")
	probe := strings.Index(Script, "--no-sandbox")
	if wrapper < 0 || probe < 0 || probe > wrapper {
		t.Fatal("the sandbox relaxation is not confined before the lease browser wrapper")
	}
}

// The Docker fallback installs docker-cli beside docker.io and names the
// distro difference instead of silently accepting a missing CLI.
func TestDockerFallbackInstallsTheClientPackage(t *testing.T) {
	for _, want := range []string{
		"apt-get install -y --no-install-recommends docker.io docker-cli",
		"apt-get install -y --no-install-recommends docker.io",
		"docker-cli is not published by this distro",
	} {
		if !strings.Contains(Script, want) {
			t.Fatalf("docker fallback missing %q", want)
		}
	}
}

// Under a systemd image the bootstrap must return rather than replace PID 1;
// the direct ending stays an exec so the container dies with sshd.
func TestSystemdEndingReturnsAndDirectEndingExecs(t *testing.T) {
	if !strings.Contains(Script, `${CRABBOX_INIT_MODE:-direct}" = "systemd"`) {
		t.Fatal("bootstrap script has no systemd init-mode branch")
	}
	if !strings.Contains(Script, "[ -d /run/systemd/system ]") {
		t.Fatal("systemd branch is not guarded by a live systemd marker")
	}
	if !strings.Contains(Script, "exec /usr/sbin/sshd -D -e -p \"$ssh_port\"") {
		t.Fatal("direct init mode must still exec sshd as the container process")
	}
	systemdIndex := strings.Index(Script, `${CRABBOX_INIT_MODE:-direct}" = "systemd"`)
	execIndex := strings.LastIndex(Script, `exec /usr/sbin/sshd -D -e -p "$ssh_port"`)
	if systemdIndex < 0 || execIndex < systemdIndex {
		t.Fatal("the systemd branch must be decided before the direct exec")
	}
}
