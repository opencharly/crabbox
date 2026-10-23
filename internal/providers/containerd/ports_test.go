package containerd

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

// liveSSHRunner models a forward child that stays alive until its context is
// canceled (or the test forces an unexpected exit). Command calls answer
// immediately, as the real nerdctl/ssh probes would.
type liveSSHRunner struct {
	mu    sync.Mutex
	calls []core.LocalCommandRequest
	exit  chan struct{}
}

func newLiveSSHRunner() *liveSSHRunner {
	return &liveSSHRunner{exit: make(chan struct{})}
}

func (r *liveSSHRunner) Run(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	r.mu.Lock()
	r.calls = append(r.calls, req)
	r.mu.Unlock()
	if req.Name == "ssh" && findArg(req.Args, "-N") {
		select {
		case <-ctx.Done():
			return core.LocalCommandResult{ExitCode: 1}, context.Cause(ctx)
		case <-r.exit:
			return core.LocalCommandResult{ExitCode: 1}, errors.New("ssh exited")
		}
	}
	joined := strings.Join(req.Args, " ")
	switch {
	case strings.Contains(joined, "namespace"):
		return core.LocalCommandResult{Stdout: "default\n"}, nil
	case strings.Contains(joined, "nerdctl") && strings.Contains(joined, "info"):
		output, err := jsonInfo(daemonInfo{ID: "daemon-id", Namespace: "default"})
		return core.LocalCommandResult{Stdout: output}, err
	}
	return core.LocalCommandResult{}, nil
}

func (r *liveSSHRunner) recorded() []core.LocalCommandRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]core.LocalCommandRequest(nil), r.calls...)
}

func (r *liveSSHRunner) crash() { close(r.exit) }

func TestParsePublishSpec(t *testing.T) {
	for _, tc := range []struct {
		spec          string
		wantContainer int
		wantLocal     int
		wantErr       string
	}{
		{spec: "3000", wantContainer: 3000},
		{spec: "41000:3000", wantContainer: 3000, wantLocal: 41000},
		{spec: " ", wantErr: "empty port mapping"},
		{spec: "0", wantErr: "1..65535"},
		{spec: "70000", wantErr: "1..65535"},
		{spec: "127.0.0.1:41000:3000", wantErr: "loopback forwards only"},
		{spec: "41000:3000/tcp", wantErr: "1..65535"},
	} {
		t.Run(tc.spec, func(t *testing.T) {
			containerPort, localPort, err := parsePublishSpec(tc.spec)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err=%v want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if containerPort != tc.wantContainer || localPort != tc.wantLocal {
				t.Fatalf("got container=%d local=%d want container=%d local=%d", containerPort, localPort, tc.wantContainer, tc.wantLocal)
			}
		})
	}
}

// The local lifecycle: publish owns a record + lock, list reports it beside the
// nerdctl mappings, and unpublish stops the owner and removes the record.
func TestPublishListAndUnpublishOwnOneForward(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const leaseID = "cbx_0123456789ab"
	runner := &recordingRunner{run: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		if findArg(req.Args, "port") {
			return core.LocalCommandResult{Stdout: "127.0.0.1:2222->22/tcp\n"}, nil
		}
		return core.LocalCommandResult{}, nil
	}}
	b := testBackend(runner)
	b.remember(core.LeaseTarget{LeaseID: leaseID, Server: core.Server{Name: "crabbox-test-00000000"}})

	var release func()
	spawned := 0
	b.spawnOwner = func(spec, logPath string) (int, <-chan error, error) {
		containerPort, requestedLocal, err := parsePublishSpec(spec)
		if err != nil {
			return 0, nil, err
		}
		if requestedLocal == 0 {
			t.Fatalf("the local lifecycle test publishes an explicit port, got %q", spec)
		}
		recordPath := strings.TrimSuffix(logPath, ".log") + ".json"
		release, err = lockForwardOwner(strings.TrimSuffix(logPath, ".log") + ".lock")
		if err != nil {
			return 0, nil, err
		}
		if err := writeForwardRecord(recordPath, publishedForward{
			LeaseID:       leaseID,
			ContainerPort: containerPort,
			LocalPort:     requestedLocal,
			OwnerPID:      os.Getpid(),
			Host:          "127.0.0.1",
			HostScope:     "local",
			CreatedAt:     time.Now().UTC(),
		}); err != nil {
			return 0, nil, err
		}
		spawned++
		return os.Getpid(), make(chan error), nil
	}
	b.stopOwner = func(pid int) error {
		if pid != os.Getpid() {
			t.Fatalf("stopped pid %d, want the recorded owner %d", pid, os.Getpid())
		}
		if release != nil {
			release()
			release = nil
		}
		return nil
	}

	out, err := b.Ports(t.Context(), core.PortsRequest{ID: leaseID, Publish: []string{"41000:3000"}})
	if err != nil {
		t.Fatal(err)
	}
	if spawned != 1 || !strings.Contains(out, "127.0.0.1:41000 -> container:3000") {
		t.Fatalf("publish output=%q spawned=%d", out, spawned)
	}

	// Publishing the same container port is idempotent and spawns nothing.
	repeat, err := b.Ports(t.Context(), core.PortsRequest{ID: leaseID, Publish: []string{"3000"}})
	if err != nil {
		t.Fatal(err)
	}
	if spawned != 1 || !strings.Contains(repeat, "127.0.0.1:41000") {
		t.Fatalf("re-publish output=%q spawned=%d", repeat, spawned)
	}
	// A different local port for an already-published container port is refused.
	if _, err := b.Ports(t.Context(), core.PortsRequest{ID: leaseID, Publish: []string{"41001:3000"}}); err == nil || !strings.Contains(err.Error(), "already published") {
		t.Fatalf("err=%v want the already-published refusal", err)
	}

	list, err := b.Ports(t.Context(), core.PortsRequest{ID: leaseID})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"127.0.0.1:2222->22/tcp", "forward 127.0.0.1:41000 -> container:3000", "lease " + leaseID} {
		if !strings.Contains(list, want) {
			t.Fatalf("list output=%q missing %q", list, want)
		}
	}

	unpublished, err := b.Ports(t.Context(), core.PortsRequest{ID: leaseID, Unpublish: []string{"3000"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(unpublished, "container:3000") {
		t.Fatalf("unpublish output=%q", unpublished)
	}
	recordPath, err := forwardRecordPath(leaseID, 3000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(recordPath); !os.IsNotExist(err) {
		t.Fatalf("the record survived unpublish: %v", err)
	}
	if _, err := b.Ports(t.Context(), core.PortsRequest{ID: leaseID, Unpublish: []string{"3000"}}); err == nil || !strings.Contains(err.Error(), "no published forward") {
		t.Fatalf("err=%v want a no-forward refusal", err)
	}
}

// R7's remote control: with SSHHost set, the published forward must ride the
// lease tunnel (destination 127.0.0.1:<tunnel-local>, -L to the container port)
// and the operator must never probe the remote host's published port on its own
// loopback. The published local port is chosen by the owner AFTER the tunnel.
func TestRemotePublishForwardsThroughTheLeaseTunnelWithoutProbingTheRemotePort(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const (
		leaseID       = "cbx_0123456789ab"
		remotePublish = "45000"
		containerPort = 3000
		requestedPort = "41000"
		tunnelPort    = "45001"
	)
	runner := newLiveSSHRunner()
	b := testBackendWithRunner(runner)
	b.cfg.Containerd.SSHHost = "containerd.example.test"
	b.cfg.Containerd.SSHUser = "operator"
	labels := map[string]string{
		"provider":       providerName,
		"lease":          leaseID,
		"container":      "crabbox-blue-lobster-00000000",
		"published_port": remotePublish,
		"host":           hostScopeForConfig(b.cfg),
		"namespace":      b.cfg.Containerd.Namespace,
		"daemon":         "daemon-id",
		"state":          "ready",
	}
	if _, err := core.ClaimLeaseTargetForRepoConfigScopeIfUnchangedDurable(
		leaseID, "blue-lobster", b.cfg, Provider{}.ClaimScope(b.cfg),
		core.Server{CloudID: "container-id", Name: labels["container"], Status: "ready", Labels: labels},
		core.SSHTarget{}, t.TempDir(), time.Hour, false, core.LeaseClaim{}, false,
	); err != nil {
		t.Fatal(err)
	}
	b.loopbackPort = func() (string, error) { return tunnelPort, nil }
	var dialMu sync.Mutex
	var dials []string
	b.dialPort = func(addr string, _ time.Duration) (net.Conn, error) {
		dialMu.Lock()
		dials = append(dials, addr)
		dialMu.Unlock()
		return &readableConn{}, nil
	}
	var waitedForSSH atomic.Bool
	b.waitSSH = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error {
		waitedForSSH.Store(true)
		return nil
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := b.Ports(ctx, core.PortsRequest{ID: leaseID, ForwardOwnerSpec: requestedPort + ":3000"})
		done <- err
	}()
	recordPath, err := forwardRecordPath(leaseID, containerPort)
	if err != nil {
		t.Fatal(err)
	}
	record := awaitForwardRecordOrExit(t, recordPath, done)
	recorded := runner.recorded()

	if !waitedForSSH.Load() {
		t.Fatal("the remote owner resolved the lease without waiting for its SSH contract")
	}
	if record.HostScope != b.cfg.Containerd.SSHHost {
		t.Fatalf("record host_scope=%q want %q", record.HostScope, b.cfg.Containerd.SSHHost)
	}
	if record.LocalPort != 41000 {
		t.Fatalf("record local_port=%d want the requested 41000", record.LocalPort)
	}
	dialMu.Lock()
	observed := append([]string(nil), dials...)
	dialMu.Unlock()
	for _, addr := range observed {
		if addr == "127.0.0.1:"+remotePublish {
			t.Fatalf("the remote published port %s was probed on the operator's loopback", addr)
		}
	}
	if !containsString(observed, "127.0.0.1:"+requestedPort) {
		t.Fatalf("the operator-side forward port %s was never probed: %v", requestedPort, observed)
	}

	tunnelLocal := ""
	for _, call := range recorded {
		if call.Name != "ssh" {
			continue
		}
		joined := strings.Join(call.Args, " ")
		if strings.HasSuffix(joined, "operator@containerd.example.test") && strings.Contains(joined, ":"+remotePublish) {
			tunnelLocal = forwardedLocalPort(call.Args)
		}
	}
	if tunnelLocal != tunnelPort {
		t.Fatalf("the lease tunnel bound %q, want the injected %q: %+v", tunnelLocal, tunnelPort, recorded)
	}
	publish := ""
	for _, call := range recorded {
		if call.Name != "ssh" {
			continue
		}
		joined := strings.Join(call.Args, " ")
		if strings.Contains(joined, "-L 127.0.0.1:"+requestedPort+":127.0.0.1:3000") {
			publish = joined
		}
	}
	if publish == "" {
		t.Fatalf("no publish forward to the container port was recorded: %+v", recorded)
	}
	if !strings.Contains(publish, "-p "+tunnelPort) || !strings.Contains(publish, "crabbox@127.0.0.1") {
		t.Fatalf("the publish forward does not ride the lease tunnel (tunnel-local %s): %s", tunnelPort, publish)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("the owner returned an error after cancellation: %v", err)
	}
	if _, err := os.Stat(recordPath); !os.IsNotExist(err) {
		t.Fatalf("the record survived the owner's teardown: %v", err)
	}
}

// The published port is chosen after the lease tunnel exists; an explicit port
// that names the tunnel would otherwise let the tunnel's own listener satisfy
// the readiness probe and silently publish the lease's SSH port.
func TestForwardOwnerRefusesAPortThatIsTheLeaseTunnel(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const leaseID = "cbx_0123456789ab"
	b := testBackendWithRunner(newLiveSSHRunner())
	b.cfg.Containerd.SSHHost = "containerd.example.test"
	b.cfg.Containerd.SSHUser = "operator"
	labels := map[string]string{
		"container": "crabbox-blue-lobster-00000000", "published_port": "45000",
		"host": hostScopeForConfig(b.cfg), "namespace": b.cfg.Containerd.Namespace,
		"daemon": "daemon-id", "state": "ready",
	}
	if _, err := core.ClaimLeaseTargetForRepoConfigScopeIfUnchangedDurable(
		leaseID, "blue-lobster", b.cfg, Provider{}.ClaimScope(b.cfg),
		core.Server{CloudID: "container-id", Name: labels["container"], Status: "ready", Labels: labels},
		core.SSHTarget{}, t.TempDir(), time.Hour, false, core.LeaseClaim{}, false,
	); err != nil {
		t.Fatal(err)
	}
	b.loopbackPort = func() (string, error) { return "41234", nil }
	b.dialPort = func(string, time.Duration) (net.Conn, error) { return &readableConn{}, nil }
	b.waitSSH = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error { return nil }
	_, err := b.Ports(t.Context(), core.PortsRequest{ID: leaseID, ForwardOwnerSpec: "41234:3000"})
	if err == nil || !strings.Contains(err.Error(), "lease's own SSH port") {
		t.Fatalf("err=%v want the lease-tunnel collision refusal", err)
	}
}

// A forward child that dies while its owner is live must not leave a record
// that reports a listener nobody serves.
func TestForwardOwnerRemovesTheRecordWhenTheSSHChildExits(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const leaseID = "cbx_0123456789ab"
	runner := newLiveSSHRunner()
	b := testBackendWithRunner(runner)
	b.remember(core.LeaseTarget{
		LeaseID: leaseID,
		Server:  core.Server{Name: "crabbox-test-00000000"},
		SSH:     core.SSHTarget{User: "crabbox", Host: "127.0.0.1", Port: "2222"},
	})
	b.loopbackPort = func() (string, error) { return "41000", nil }
	b.dialPort = func(string, time.Duration) (net.Conn, error) { return &readableConn{}, nil }

	done := make(chan error, 1)
	go func() {
		_, err := b.Ports(t.Context(), core.PortsRequest{ID: leaseID, ForwardOwnerSpec: "3000"})
		done <- err
	}()
	recordPath, err := forwardRecordPath(leaseID, 3000)
	if err != nil {
		t.Fatal(err)
	}
	awaitForwardRecordOrExit(t, recordPath, done)
	runner.crash()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "exited unexpectedly") {
			t.Fatalf("err=%v want an unexpected-exit error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the owner did not notice its ssh child exiting")
	}
	if _, err := os.Stat(recordPath); !os.IsNotExist(err) {
		t.Fatalf("the record survived the child's exit: %v", err)
	}
}

// The owner's shutdown path is the process context the binary cancels on
// SIGTERM (cmd/crabbox/main.go's signal.NotifyContext); canceling it here
// proves the same teardown removes the record.
func TestForwardOwnerRemovesTheRecordOnCancellation(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const leaseID = "cbx_0123456789ab"
	b := testBackendWithRunner(newLiveSSHRunner())
	b.remember(core.LeaseTarget{
		LeaseID: leaseID,
		Server:  core.Server{Name: "crabbox-test-00000000"},
		SSH:     core.SSHTarget{User: "crabbox", Host: "127.0.0.1", Port: "2222"},
	})
	b.loopbackPort = func() (string, error) { return "41000", nil }
	b.dialPort = func(string, time.Duration) (net.Conn, error) { return &readableConn{}, nil }

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := b.Ports(ctx, core.PortsRequest{ID: leaseID, ForwardOwnerSpec: "3000"})
		done <- err
	}()
	recordPath, err := forwardRecordPath(leaseID, 3000)
	if err != nil {
		t.Fatal(err)
	}
	awaitForwardRecordOrExit(t, recordPath, done)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("canceled owner returned an error: %v", err)
	}
	if _, err := os.Stat(recordPath); !os.IsNotExist(err) {
		t.Fatalf("the record survived cancellation: %v", err)
	}
}

// Release reaps a live published forward: release calls reapPublishedForwards,
// which stops the owner and removes its state.
func TestReapPublishedForwardsStopsTheOwnerAndRemovesState(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const leaseID = "cbx_0123456789ab"
	b := testBackend(&recordingRunner{})
	recordPath, err := forwardRecordPath(leaseID, 3000)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(recordPath), 0o700); err != nil {
		t.Fatal(err)
	}
	release, err := lockForwardOwner(forwardLockPathFor(recordPath))
	if err != nil {
		t.Fatal(err)
	}
	if err := writeForwardRecord(recordPath, publishedForward{
		LeaseID: leaseID, ContainerPort: 3000, LocalPort: 41000, OwnerPID: 111,
		Host: "127.0.0.1", HostScope: "local", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	stopped := 0
	b.stopOwner = func(pid int) error {
		stopped++
		release()
		return nil
	}
	if err := b.reapPublishedForwards(t.Context(), leaseID); err != nil {
		t.Fatal(err)
	}
	if stopped != 1 {
		t.Fatalf("release reaped %d owners, want 1", stopped)
	}
	if _, err := os.Stat(recordPath); !os.IsNotExist(err) {
		t.Fatalf("release left the forward record: %v", err)
	}
}

// A concurrent publisher that takes the port between the stop signal and the
// state removal must keep its record: the remover proves ownership under the
// lock and leaves a re-owned forward alone.
func TestUnpublishLeavesAForwardReownedDuringStop(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const leaseID = "cbx_0123456789ab"
	b := testBackend(&recordingRunner{})
	b.remember(core.LeaseTarget{LeaseID: leaseID, Server: core.Server{Name: "crabbox-test-00000000"}})
	recordPath, err := forwardRecordPath(leaseID, 3000)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(recordPath), 0o700); err != nil {
		t.Fatal(err)
	}
	lockPath := forwardLockPathFor(recordPath)
	oldRelease, err := lockForwardOwner(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeForwardRecord(recordPath, publishedForward{
		LeaseID: leaseID, ContainerPort: 3000, LocalPort: 41000, OwnerPID: 111,
		Host: "127.0.0.1", HostScope: "local", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	var newRelease func()
	b.stopOwner = func(pid int) error {
		oldRelease()
		release, err := lockForwardOwner(lockPath)
		if err != nil {
			return err
		}
		newRelease = release
		return writeForwardRecord(recordPath, publishedForward{
			LeaseID: leaseID, ContainerPort: 3000, LocalPort: 41001, OwnerPID: 222,
			Host: "127.0.0.1", HostScope: "local", CreatedAt: time.Now().UTC(),
		})
	}
	defer func() {
		if newRelease != nil {
			newRelease()
		}
	}()

	_, err = b.Ports(t.Context(), core.PortsRequest{ID: leaseID, Unpublish: []string{"3000"}})
	if err == nil || !strings.Contains(err.Error(), "re-owned") {
		t.Fatalf("err=%v want the re-owned refusal", err)
	}
	record, ok, err := readForwardRecord(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || record.OwnerPID != 222 {
		t.Fatalf("the new owner's record was not preserved: ok=%v record=%+v", ok, record)
	}
}

// Concurrent --publish calls for one container port must leave exactly one
// owner: the owner lock, not a pre-check read, is what serializes them.
func TestConcurrentPublishOfOneContainerPortHasOneOwner(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const leaseID = "cbx_0123456789ab"
	b := testBackend(&recordingRunner{})
	b.remember(core.LeaseTarget{LeaseID: leaseID, Server: core.Server{Name: "crabbox-test-00000000"}})
	var mu sync.Mutex
	var releases []func()
	b.spawnOwner = func(spec, logPath string) (int, <-chan error, error) {
		containerPort, requestedLocal, err := parsePublishSpec(spec)
		if err != nil {
			return 0, nil, err
		}
		release, err := lockForwardOwner(strings.TrimSuffix(logPath, ".log") + ".lock")
		if err != nil {
			return 0, nil, err
		}
		mu.Lock()
		releases = append(releases, release)
		mu.Unlock()
		if err := writeForwardRecord(strings.TrimSuffix(logPath, ".log")+".json", publishedForward{
			LeaseID: leaseID, ContainerPort: containerPort, LocalPort: requestedLocal, OwnerPID: os.Getpid(),
			Host: "127.0.0.1", HostScope: "local", CreatedAt: time.Now().UTC(),
		}); err != nil {
			return 0, nil, err
		}
		return os.Getpid(), make(chan error), nil
	}
	defer func() {
		for _, release := range releases {
			release()
		}
	}()
	errs := make(chan error, 2)
	for index := 0; index < 2; index++ {
		go func(local string) {
			_, err := b.Ports(t.Context(), core.PortsRequest{ID: leaseID, Publish: []string{local + ":3000"}})
			errs <- err
		}(strconv.Itoa(41000 + index))
	}
	first, second := <-errs, <-errs
	if first != nil && second != nil {
		t.Fatalf("both concurrent publishes failed: %v / %v", first, second)
	}
	mu.Lock()
	owners := len(releases)
	mu.Unlock()
	if owners != 1 {
		t.Fatalf("spawned %d forward owners for one container port", owners)
	}
}

// Liveness is the owner lock, never the recorded pid: a stale record whose pid
// has been recycled must not be reported and must not be signalled.
func TestStaleForwardRecordIsNotReportedOrSignalled(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const leaseID = "cbx_0123456789ab"
	runner := &recordingRunner{run: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		if findArg(req.Args, "port") {
			return core.LocalCommandResult{Stdout: "127.0.0.1:2222->22/tcp\n"}, nil
		}
		return core.LocalCommandResult{}, nil
	}}
	b := testBackend(runner)
	b.remember(core.LeaseTarget{LeaseID: leaseID, Server: core.Server{Name: "crabbox-test-00000000"}})
	recordPath, err := forwardRecordPath(leaseID, 3000)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(recordPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeForwardRecord(recordPath, publishedForward{
		LeaseID: leaseID, ContainerPort: 3000, LocalPort: 41000, OwnerPID: os.Getpid(),
		Host: "127.0.0.1", HostScope: "local", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	output, err := b.Ports(t.Context(), core.PortsRequest{ID: leaseID})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output, "container:3000") {
		t.Fatalf("a stale record was reported as a live forward: %q", output)
	}
	if _, err := os.Stat(recordPath); !os.IsNotExist(err) {
		t.Fatalf("the stale record was not removed: %v", err)
	}
}

// A forward whose lease side closes the connection must name the lease side and
// the containerd host, never blame the operator's own loopback.
func TestProbeNamesTheLeaseSideWhenTheForwardCloses(t *testing.T) {
	b := testBackend(&recordingRunner{})
	b.cfg.Containerd.SSHHost = "containerd.example.test"
	b.dialPort = func(string, time.Duration) (net.Conn, error) { return closedConn{}, nil }
	err := b.probePublishedForward(t.Context(), core.LeaseTarget{LeaseID: "cbx_0123456789ab"}, 3000, 41000)
	if err == nil {
		t.Fatal("expected the closed forward to be refused")
	}
	for _, want := range []string{"inside lease cbx_0123456789ab", "containerd.example.test", "127.0.0.1:3000"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err=%v missing %q", err, want)
		}
	}
}

func TestWithEnvReplacesInsteadOfDuplicating(t *testing.T) {
	got := withEnv([]string{"A=1", "B=2"}, map[string]string{"B": "3", "C": "4"})
	joined := strings.Join(got, " ")
	if strings.Contains(joined, "B=2") {
		t.Fatalf("the stale value survived: %v", got)
	}
	for _, want := range []string{"A=1", "B=3", "C=4"} {
		if !containsString(got, want) {
			t.Fatalf("%v missing %q", got, want)
		}
	}
}

func awaitForwardRecordOrExit(t *testing.T, path string, done <-chan error) publishedForward {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			t.Fatalf("the forward owner returned before publishing its record: %v", err)
		default:
		}
		record, ok, err := readForwardRecord(path)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			return record
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the forward owner never published %s", path)
	return publishedForward{}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func forwardedLocalPort(args []string) string {
	for index, arg := range args {
		if arg != "-L" || index+1 >= len(args) {
			continue
		}
		parts := strings.Split(args[index+1], ":")
		if len(parts) >= 2 {
			return parts[1]
		}
	}
	return ""
}

// readableConn answers a read immediately, standing in for a lease listener
// that accepts the forwarded connection.
type readableConn struct{}

func (readableConn) Read(buffer []byte) (int, error) {
	if len(buffer) > 0 {
		buffer[0] = 'S'
		return 1, nil
	}
	return 0, io.EOF
}
func (readableConn) Write(buffer []byte) (int, error) { return len(buffer), nil }
func (readableConn) Close() error                     { return nil }
func (readableConn) LocalAddr() net.Addr              { return nil }
func (readableConn) RemoteAddr() net.Addr             { return nil }
func (readableConn) SetDeadline(time.Time) error      { return nil }
func (readableConn) SetReadDeadline(time.Time) error  { return nil }
func (readableConn) SetWriteDeadline(time.Time) error { return nil }

// closedConn behaves like a local listener whose lease-side connect was
// refused: the connection dies at the first read.
type closedConn struct{ readableConn }

func (closedConn) Read([]byte) (int, error) { return 0, io.EOF }
