package containerd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestLocalInputsArePrivateAndAtomic(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	b := testBackend(&recordingRunner{})
	dir, err := b.prepareInputs(t.Context(), "cbx_lease", "ssh-ed25519 AAAA test@example")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dir.dir, filepath.Join(state, "crabbox", "containerd", "leases", "cbx_lease")) {
		t.Fatalf("inputs dir %q is outside the state root", dir.dir)
	}
	info, err := os.Stat(dir.dir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("inputs dir mode=%v err=%v", info.Mode().Perm(), err)
	}
	for _, name := range []string{"authorized_keys", "lease.env", "bootstrap.sh"} {
		path := filepath.Join(dir.dir, name)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode=%v want 0600", name, info.Mode().Perm())
		}
	}
	entries, err := os.ReadDir(dir.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp.") {
			t.Fatalf("temporary file %s survived the rename", entry.Name())
		}
	}
	leaseEnv, err := os.ReadFile(filepath.Join(dir.dir, "lease.env"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"CRABBOX_LEASE_ID=cbx_lease", "CRABBOX_SSH_USER=crabbox", "CRABBOX_SSH_PORT=2222", "CRABBOX_NAMESPACE=default"} {
		if !strings.Contains(string(leaseEnv), want) {
			t.Fatalf("lease.env missing %q: %s", want, leaseEnv)
		}
	}
	bootstrap, err := os.ReadFile(filepath.Join(dir.dir, "bootstrap.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(bootstrap), "exec /usr/sbin/sshd -D -e -p \"$ssh_port\"") {
		t.Fatalf("bootstrap.sh is not the shared container bootstrap")
	}
}

func TestReleaseReportsLeakWhenContainerStillListed(t *testing.T) {
	runner := &recordingRunner{run: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		joined := strings.Join(req.Args, " ")
		switch {
		case strings.Contains(joined, " info "):
			return core.LocalCommandResult{Stdout: `{"ID":"daemon-id","SecurityOptions":["name=seccomp"]}`}, nil
		case strings.Contains(joined, "namespace ls"):
			return core.LocalCommandResult{Stdout: "default\n"}, nil
		case strings.Contains(joined, "rm -f"):
			return core.LocalCommandResult{Stdout: "name\n"}, nil
		case strings.Contains(joined, "ps -a") && strings.Contains(joined, "name=^name$"):
			return core.LocalCommandResult{Stdout: "name\n"}, nil
		case strings.Contains(joined, "stop"):
			return core.LocalCommandResult{ExitCode: 0}, nil
		}
		return core.LocalCommandResult{}, nil
	}}
	b := testBackend(runner)
	lease := core.LeaseTarget{
		LeaseID: "cbx_lease",
		Server:  core.Server{Name: "name", Labels: map[string]string{"state_volume": "crabbox-cbx_lease-state", "inputs_dir": "/tmp/inputs"}},
	}
	err := b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease})
	if err == nil || !strings.Contains(err.Error(), "still listed") {
		t.Fatalf("rm -f exit 0 with the container still listed must be a reported leak; err=%v", err)
	}
}

func TestDoctorReportsCRIUAutoNarrowing(t *testing.T) {
	runner := &recordingRunner{run: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		joined := strings.Join(req.Args, " ")
		switch {
		case strings.Contains(joined, "--version"):
			return core.LocalCommandResult{Stdout: "nerdctl version 2.2.0"}, nil
		case strings.Contains(joined, "info"):
			return core.LocalCommandResult{Stdout: `{"ID":"daemon-id","SecurityOptions":["name=seccomp"]}`}, nil
		case strings.Contains(joined, "namespace ls"):
			return core.LocalCommandResult{Stdout: "default\n"}, nil
		case strings.Contains(joined, "ps -a"):
			return core.LocalCommandResult{}, nil
		case strings.Contains(joined, "test -e"):
			return core.LocalCommandResult{ExitCode: 0}, nil
		}
		return core.LocalCommandResult{}, nil
	}}
	b := testBackend(runner)
	result, err := b.Doctor(t.Context(), core.DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, check := range result.Checks {
		if check.Check == "criu" && strings.Contains(check.Message, "narrows to off") {
			found = true
		}
	}
	if !found {
		t.Fatalf("criu auto narrowing must be reported, never silent: %+v", result.Checks)
	}
}

func TestNerdctlGlobalArgsOrder(t *testing.T) {
	b := testBackend(&recordingRunner{})
	args := b.runner().globalArgs()
	want := []string{"-n", "default", "--snapshotter", "overlayfs", "--address", "/run/containerd/containerd.sock"}
	if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("global args=%v want %v", args, want)
	}
}

// A stock OCI image never ships the prepared-image marker
// /usr/local/bin/crabbox-ready, so the generic readiness probe would wait for a
// file the lease can never have until the deadline. The lease's own readiness
// contract is the work root its bootstrap created.
func TestLeaseReadinessIsTheBootstrapWorkRoot(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	b := testBackend(&recordingRunner{})
	target, err := b.sshTargetForPort(t.Context(), "cbx_lease", "34457")
	if err != nil {
		t.Fatal(err)
	}
	if target.ReadyCheck == "" {
		t.Fatal("lease ReadyCheck is empty; the generic probe then requires the prepared-image marker /usr/local/bin/crabbox-ready, which a stock image does not ship")
	}
	if strings.TrimSpace(b.cfg.Containerd.WorkRoot) == "" {
		t.Fatal("test backend has no work root; the readiness assertion would be vacuous")
	}
	if !strings.Contains(target.ReadyCheck, b.cfg.Containerd.WorkRoot) {
		t.Fatalf("ReadyCheck %q does not assert the lease work root %q", target.ReadyCheck, b.cfg.Containerd.WorkRoot)
	}
}
