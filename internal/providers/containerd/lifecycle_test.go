package containerd

import (
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
