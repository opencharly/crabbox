package containerd

import (
	"errors"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

// Acquire refuses a caller-supplied lease ID before any daemon call, because
// the provider does not advertise fixed-lease IDs.
func TestAcquireRejectsRequestedLeaseIDBeforeTouchingTheDaemon(t *testing.T) {
	runner := &recordingRunner{}
	b := testBackend(runner)
	_, err := b.Acquire(t.Context(), core.AcquireRequest{RequestedLeaseID: "cbx_fixed"})
	if err == nil || !strings.Contains(err.Error(), "does not support caller-supplied lease IDs") {
		t.Fatalf("err=%v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("Acquire touched the daemon before refusing: %+v", runner.calls)
	}
}

// List keeps only this provider's labels for the configured host and namespace.
func TestListFiltersByProviderHostAndNamespace(t *testing.T) {
	objects := map[string]string{
		"a": `[{"Id":"a","Name":"/one","State":{"Status":"running"},"Config":{"Labels":{"provider":"containerd","lease":"cbx_a","host":"local","namespace":"default","state":"ready"}}}]`,
		"b": `[{"Id":"b","Name":"/two","State":{"Status":"running"},"Config":{"Labels":{"provider":"local-container","lease":"cbx_b","host":"local","namespace":"default","state":"ready"}}}]`,
		"c": `[{"Id":"c","Name":"/three","State":{"Status":"running"},"Config":{"Labels":{"provider":"containerd","lease":"cbx_c","host":"other.example.test","namespace":"default","state":"ready"}}}]`,
		"d": `[{"Id":"d","Name":"/four","State":{"Status":"running"},"Config":{"Labels":{"provider":"containerd","lease":"cbx_d","host":"local","namespace":"k8s.io","state":"ready"}}}]`,
	}
	runner := &recordingRunner{run: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		joined := strings.Join(req.Args, " ")
		if strings.Contains(joined, "ps -a") {
			return core.LocalCommandResult{Stdout: "a\nb\nc\nd"}, nil
		}
		for id, object := range objects {
			if strings.HasSuffix(joined, " "+id) {
				// The real CLI emits a JSON array without --format and one object
				// per line with it; the decoder must be fed the real shape for the
				// argv it actually runs.
				if findArg(req.Args, "--format") {
					return core.LocalCommandResult{Stdout: strings.TrimSuffix(strings.TrimPrefix(object, "["), "]")}, nil
				}
				return core.LocalCommandResult{Stdout: object}, nil
			}
		}
		return core.LocalCommandResult{}, nil
	}}
	b := testBackend(runner)
	views, err := b.List(t.Context(), core.ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].Name != "one" {
		t.Fatalf("views=%+v want only the local/default containerd lease", views)
	}
}

// A failed `nerdctl run` must remove the state volume it created: nerdctl
// creates the named volume before the container start, so a container-only
// rollback leaks it.
func TestAcquireFailedRunRollsBackTheStateVolume(t *testing.T) {
	for _, tc := range []struct {
		name        string
		stderr      string
		wantPortmap bool
	}{
		{"generic run failure", "boom", false},
		{"portmap host limit", "iptables -t nat -A CNI-HOSTPORT-DNAT -m multiport: Extension multiport revision 0 not supported", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			runner := &recordingRunner{run: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
				switch {
				case findArg(req.Args, "info"):
					output, _ := jsonInfo(daemonInfo{ID: "daemon-id", Namespace: "default"})
					return core.LocalCommandResult{Stdout: output}, nil
				case findArg(req.Args, "namespace", "ls"):
					return core.LocalCommandResult{Stdout: "default\n"}, nil
				case findArg(req.Args, "ps"):
					return core.LocalCommandResult{}, nil
				case findArg(req.Args, "run"):
					return core.LocalCommandResult{ExitCode: 1, Stderr: tc.stderr}, errors.New("exit status 1")
				}
				return core.LocalCommandResult{}, nil
			}}
			b := testBackend(runner)
			_, err := b.Acquire(t.Context(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}})
			if err == nil {
				t.Fatal("expected the failed run to be reported")
			}
			if strings.Contains(err.Error(), "xt_multiport") != tc.wantPortmap {
				t.Fatalf("err=%v wantPortmap=%v", err, tc.wantPortmap)
			}
			volume := ""
			for _, call := range runner.calls {
				for i := 0; i+1 < len(call.Args); i++ {
					if call.Args[i] == "-v" && strings.HasSuffix(call.Args[i+1], ":/var") {
						volume = strings.TrimSuffix(call.Args[i+1], ":/var")
					}
				}
			}
			if volume == "" {
				t.Fatal("the lease did not request a state volume")
			}
			if !hasRecordedCall(runner.calls, "volume", "rm", "--force", volume) {
				t.Fatalf("failed acquire leaked the state volume %s", volume)
			}
			if !hasRecordedCall(runner.calls, "rm", "-f") {
				t.Fatal("failed acquire did not remove the container")
			}
		})
	}
}

// nerdctl remove.go reports success on a failed removal, so release must prove
// the container is gone by listing it rather than trusting `rm -f`'s exit code.
func TestReleaseReportsALeakWhenRMExitsZeroAndTheContainerRemains(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const name = "crabbox-blue-lobster-00000000"
	runner := &recordingRunner{run: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		switch {
		case findArg(req.Args, "info"):
			output, _ := jsonInfo(daemonInfo{ID: "daemon-id", Namespace: "default"})
			return core.LocalCommandResult{Stdout: output}, nil
		case findArg(req.Args, "namespace", "ls"):
			return core.LocalCommandResult{Stdout: "default\n"}, nil
		case findArg(req.Args, "stop"):
			return core.LocalCommandResult{}, nil
		case findArg(req.Args, "rm", "-f"):
			return core.LocalCommandResult{}, nil
		case findArg(req.Args, "ps", "-a", "--filter", "name=^"+name+"$"):
			return core.LocalCommandResult{Stdout: name + "\n"}, nil
		}
		return core.LocalCommandResult{}, nil
	}}
	err := testBackend(runner).ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{
		LeaseID: "cbx_0123456789ab",
		Server:  core.Server{Name: name},
	}})
	if err == nil || !strings.Contains(err.Error(), "still listed") {
		t.Fatalf("err=%v, want a reported leak rather than a successful release", err)
	}
}

// A browser lease with a missing profile must refuse before any container is
// provisioned: buildRunArgs passes the path to the runtime, so a late check
// only turns a clear refusal into a run failure after a wasted start.
func TestAcquireRefusesAMissingSeccompProfileBeforeProvisioning(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	runner := &recordingRunner{run: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		switch {
		case findArg(req.Args, "info"):
			output, _ := jsonInfo(daemonInfo{ID: "daemon-id", Namespace: "default"})
			return core.LocalCommandResult{Stdout: output}, nil
		case findArg(req.Args, "namespace", "ls"):
			return core.LocalCommandResult{Stdout: "default\n"}, nil
		case req.Name == "sh" && strings.Contains(strings.Join(req.Args, " "), "test -e"):
			return core.LocalCommandResult{ExitCode: 1}, nil
		}
		return core.LocalCommandResult{}, nil
	}}
	b := testBackend(runner)
	b.cfg.Browser = true
	_, err := b.Acquire(t.Context(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}})
	if err == nil || !strings.Contains(err.Error(), "CONTAINERD.md") {
		t.Fatalf("err=%v, want the missing-profile refusal", err)
	}
	if hasRecordedCall(runner.calls, "run") {
		t.Fatal("a missing seccomp profile must be refused before nerdctl run")
	}
}

func hasRecordedCall(calls []core.LocalCommandRequest, want ...string) bool {
	for _, call := range calls {
		if findArg(call.Args, want...) {
			return true
		}
	}
	return false
}

// Resolve fails closed on an unknown claim without touching the runtime.
func TestResolveRejectsUnknownClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	runner := &recordingRunner{}
	b := testBackend(runner)
	_, err := b.Resolve(t.Context(), core.ResolveRequest{ID: "cbx_0123456789ab"})
	if err == nil {
		t.Fatal("expected an unknown-lease error")
	}
	if len(runner.calls) != 0 {
		t.Fatalf("Resolve touched the runtime for an unknown claim: %+v", runner.calls)
	}
}
