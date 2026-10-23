package containerd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/openclaw/crabbox/internal/atomicfile"
	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

const (
	forwardStateDirName        = "containerd-forwards"
	forwardOwnerStartupTimeout = 2 * time.Minute
	forwardOwnerStopTimeout    = 15 * time.Second
	forwardOwnerPollInterval   = 50 * time.Millisecond
	forwardProbeReadTimeout    = 2 * time.Second
	forwardLogTailBytes        = 4096
)

// publishedForward is the durable identity of one operator-side SSH forward.
// The owner pid is recorded so `--unpublish` and release can stop it; liveness
// is decided by the owner's exclusive lock, never by the pid alone (a recycled
// pid must never be signalled).
type publishedForward struct {
	LeaseID       string    `json:"lease_id"`
	ContainerPort int       `json:"container_port"`
	LocalPort     int       `json:"local_port"`
	OwnerPID      int       `json:"owner_pid"`
	Host          string    `json:"host"`
	HostScope     string    `json:"host_scope"`
	CreatedAt     time.Time `json:"created_at"`
}

// portMapping is one row in `ports` output. JSON output is an array of these.
type portMapping struct {
	Source        string `json:"source"`
	Mapping       string `json:"mapping,omitempty"`
	LocalHost     string `json:"local_host,omitempty"`
	LocalPort     int    `json:"local_port,omitempty"`
	ContainerPort int    `json:"container_port,omitempty"`
	OwnerPID      int    `json:"owner_pid,omitempty"`
	HostScope     string `json:"host_scope,omitempty"`
	LeaseID       string `json:"lease_id,omitempty"`
}

type spawnForwardOwnerFunc func(spec, logPath string) (int, <-chan error, error)
type stopForwardOwnerFunc func(pid int) error

var errForwardOwnerPending = errors.New("forward owner has not published its record yet")

// forwardOwnerExitError carries the child's own exit as the cause of a failed
// publish, so the caller can attach the owner's log tail.
type forwardOwnerExitError struct{ err error }

func (e *forwardOwnerExitError) Error() string {
	if e.err == nil {
		return "the forward owner exited before publishing its record"
	}
	return "the forward owner exited before publishing its record: " + e.err.Error()
}

func (e *forwardOwnerExitError) Unwrap() error { return e.err }

// Ports reads the nerdctl mappings plus live forwards, or publishes/unpublishes
// one. `--publish` never asks nerdctl to change a running container: nerdctl
// cannot, so the provider owns an SSH local forward instead.
func (b *backend) Ports(ctx context.Context, req core.PortsRequest) (string, error) {
	switch {
	case strings.TrimSpace(req.ForwardOwnerSpec) != "":
		return b.runForwardOwner(ctx, req)
	case len(req.Unpublish) > 0:
		return b.unpublishPorts(ctx, req)
	case len(req.Publish) > 0:
		return b.publishPorts(ctx, req)
	default:
		return b.listPorts(ctx, req)
	}
}

func (b *backend) listPorts(ctx context.Context, req core.PortsRequest) (string, error) {
	lease, err := b.Resolve(ctx, core.ResolveRequest{ID: req.ID})
	if err != nil {
		return "", err
	}
	result, err := b.runner().nerdctlOutput(ctx, "port", lease.Server.Name)
	if err != nil {
		return "", shared.LocalCommandError("nerdctl port", result, err)
	}
	mappings := []portMapping{}
	for _, line := range strings.Split(result.Stdout, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			mappings = append(mappings, portMapping{Source: "nerdctl", Mapping: line})
		}
	}
	forwards, err := b.livePublishedForwards(lease.LeaseID)
	if err != nil {
		return "", err
	}
	for _, forward := range forwards {
		mappings = append(mappings, forwardMapping(forward))
	}
	return renderPortMappings(req.JSON, mappings)
}

func (b *backend) publishPorts(ctx context.Context, req core.PortsRequest) (string, error) {
	if err := forwardPublishingSupported(); err != nil {
		return "", err
	}
	lease, err := b.Resolve(ctx, core.ResolveRequest{ID: req.ID})
	if err != nil {
		return "", err
	}
	mappings := []portMapping{}
	for _, spec := range req.Publish {
		containerPort, requestedLocal, err := parsePublishSpec(spec)
		if err != nil {
			return "", err
		}
		if existing, ok, err := b.livePublishedForward(lease.LeaseID, containerPort); err != nil {
			return "", err
		} else if ok {
			if requestedLocal != 0 && existing.LocalPort != requestedLocal {
				return "", core.Exit(2, "container port %d is already published on 127.0.0.1:%d for lease %s; unpublish it first or omit the local port", containerPort, existing.LocalPort, lease.LeaseID)
			}
			mappings = append(mappings, forwardMapping(existing))
			continue
		}
		// The owner allocates the operator-side port AFTER it establishes the
		// lease tunnel: allocating here lets the allocator hand the same port to
		// both, and the tunnel's listener then satisfies the publish readiness
		// probe.
		forward, err := b.startForwardOwner(ctx, lease, containerPort, spec)
		if err != nil {
			return "", err
		}
		mappings = append(mappings, forwardMapping(forward))
	}
	return renderPortMappings(req.JSON, mappings)
}

func (b *backend) unpublishPorts(ctx context.Context, req core.PortsRequest) (string, error) {
	if err := forwardPublishingSupported(); err != nil {
		return "", err
	}
	lease, err := b.Resolve(ctx, core.ResolveRequest{ID: req.ID})
	if err != nil {
		return "", err
	}
	mappings := []portMapping{}
	for _, spec := range req.Unpublish {
		containerPort, requestedLocal, err := parsePublishSpec(spec)
		if err != nil {
			return "", err
		}
		recordPath, err := forwardRecordPath(lease.LeaseID, containerPort)
		if err != nil {
			return "", err
		}
		record, ok, err := readForwardRecord(recordPath)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", core.Exit(2, "no published forward for container port %d on lease %s; `crabbox ports --provider containerd --id %s` lists the active forwards", containerPort, lease.LeaseID, req.ID)
		}
		if requestedLocal != 0 && record.LocalPort != requestedLocal {
			return "", core.Exit(2, "container port %d is published on 127.0.0.1:%d, not %d; unpublish it with --unpublish %d", containerPort, record.LocalPort, requestedLocal, containerPort)
		}
		if err := b.stopPublishedForwardOwner(ctx, lease.LeaseID, record); err != nil {
			return "", err
		}
		mappings = append(mappings, forwardMapping(record))
	}
	return renderPortMappings(req.JSON, mappings)
}

// runForwardOwner is the detached helper process. It owns one forward for its
// whole lifetime: the ssh child is its child, the record names its pid, and the
// lock it holds is the liveness proof another process reads.
func (b *backend) runForwardOwner(ctx context.Context, req core.PortsRequest) (string, error) {
	if err := forwardPublishingSupported(); err != nil {
		return "", err
	}
	containerPort, requestedLocal, err := parsePublishSpec(req.ForwardOwnerSpec)
	if err != nil {
		return "", err
	}
	// Prepare starts the lease tunnel in remote mode, so the forward below
	// addresses the operator-side end and never the remote host's loopback.
	// It runs FIRST: the tunnel binds an operator-side port, and only then can a
	// free published port be chosen without the two allocations colliding.
	lease, err := b.Resolve(ctx, core.ResolveRequest{ID: req.ID, Prepare: true})
	if err != nil {
		return "", err
	}
	// Every return below must take the lease tunnel down with it, or its ssh
	// child outlives the owner.
	defer b.stopForwardKeyAndWaitQuietly(lease.LeaseID, containerPort, true)
	localPort := requestedLocal
	if localPort == 0 {
		allocated, err := b.newLoopbackPort()
		if err != nil {
			return "", err
		}
		localPort, _ = strconv.Atoi(allocated)
	}
	// Even with the tunnel already bound, an explicitly requested port can name
	// it; its listener would satisfy the readiness probe and the forward would
	// silently serve the lease's SSH port instead of the container port.
	if strconv.Itoa(localPort) == strings.TrimSpace(lease.SSH.Port) {
		return "", core.Exit(2, "local port %d is the lease's own SSH port on this operator; choose another port or omit it", localPort)
	}
	recordPath, err := forwardRecordPath(lease.LeaseID, containerPort)
	if err != nil {
		return "", err
	}
	lockPath, err := forwardLockPath(lease.LeaseID, containerPort)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(recordPath), 0o700); err != nil {
		return "", core.Exit(5, "create the forward state directory: %v", err)
	}
	unlock, err := lockForwardOwner(lockPath)
	if err != nil {
		return "", err
	}
	defer unlock()
	// We hold the lock, so any record still on disk belongs to a dead owner.
	_ = os.Remove(recordPath)
	if err := b.startPublishedForward(ctx, lease.LeaseID, lease.SSH, strconv.Itoa(localPort), containerPort); err != nil {
		return "", err
	}
	record := publishedForward{
		LeaseID:       lease.LeaseID,
		ContainerPort: containerPort,
		LocalPort:     localPort,
		OwnerPID:      os.Getpid(),
		Host:          "127.0.0.1",
		HostScope:     b.hostScope(),
		CreatedAt:     b.now(),
	}
	if err := b.probePublishedForward(ctx, lease, containerPort, localPort); err != nil {
		return "", err
	}
	if err := writeForwardRecord(recordPath, record); err != nil {
		return "", err
	}
	fmt.Fprintf(b.rt.Stderr, "forward owner pid=%d lease=%s container_port=%d local_port=%d host_scope=%s\n",
		record.OwnerPID, record.LeaseID, containerPort, localPort, record.HostScope)
	done, ok := b.forwardDone(publishedForwardKey(lease.LeaseID, containerPort))
	if !ok {
		_ = os.Remove(recordPath)
		return "", core.Exit(5, "the publish forward for container port %d was not registered", containerPort)
	}
	select {
	case <-ctx.Done():
		return b.finishForwardOwner(ctx, req.JSON, lease, containerPort, record, recordPath)
	case <-done:
		if ctx.Err() != nil {
			// Shutdown won the race; take the normal teardown.
			return b.finishForwardOwner(ctx, req.JSON, lease, containerPort, record, recordPath)
		}
		// The ssh child died while the owner was live: do not leave a record
		// that reports a forward nobody is serving.
		b.stopForwardKeyAndWaitQuietly(lease.LeaseID, containerPort, true)
		_ = os.Remove(recordPath)
		return "", core.Exit(5, "the publish forward for container port %d exited unexpectedly; the lease SSH session ended and no listener is published", containerPort)
	}
}

// finishForwardOwner tears the owner's forwards down and removes its record
// only when the teardown is proven. A failed teardown retains the record so a
// caller sees the failure instead of a silent success with a live listener.
func (b *backend) finishForwardOwner(ctx context.Context, jsonOut bool, lease core.LeaseTarget, containerPort int, record publishedForward, recordPath string) (string, error) {
	teardownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), forwardOwnerStopTimeout)
	defer cancel()
	var errs []error
	if err := b.stopForwardKeyAndWait(teardownCtx, publishedForwardKey(lease.LeaseID, containerPort)); err != nil {
		errs = append(errs, fmt.Errorf("published forward: %w", err))
	}
	if err := b.stopForwardKeyAndWait(teardownCtx, leaseForwardKey(lease.LeaseID)); err != nil {
		errs = append(errs, fmt.Errorf("lease tunnel: %w", err))
	}
	if err := errors.Join(errs...); err != nil {
		return "", core.Exit(5, "the forward owner could not stop the forward for container port %d: %v; the record is retained", containerPort, err)
	}
	_ = os.Remove(recordPath)
	return renderPortMappings(jsonOut, []portMapping{forwardMapping(record)})
}

func (b *backend) stopForwardKeyAndWaitQuietly(leaseID string, containerPort int, withTunnel bool) {
	ctx, cancel := context.WithTimeout(context.Background(), forwardOwnerStopTimeout)
	defer cancel()
	_ = b.stopForwardKeyAndWait(ctx, publishedForwardKey(leaseID, containerPort))
	if withTunnel {
		_ = b.stopForwardKeyAndWait(ctx, leaseForwardKey(leaseID))
	}
}

// probePublishedForward proves the forward end to end before it is recorded: a
// connection the lease side refuses is closed at once, while a listener that
// accepts holds it open (or sends a banner). The message names the side that
// failed, because the port lives inside the lease, never on the operator's
// loopback in remote mode.
func (b *backend) probePublishedForward(ctx context.Context, lease core.LeaseTarget, containerPort, localPort int) error {
	conn, err := b.dial("127.0.0.1:" + strconv.Itoa(localPort))
	if err != nil {
		return core.Exit(5, "the operator-side forward on 127.0.0.1:%d for container port %d did not accept: %v", localPort, containerPort, err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(b.now().Add(forwardProbeReadTimeout))
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err == nil {
		return nil
	} else if isReadTimeout(err) {
		// The lease side accepted and held the connection open.
		return nil
	}
	return core.Exit(5, "nothing answered on 127.0.0.1:%d inside lease %s (containerd host %s): the operator-side forward on 127.0.0.1:%d is up, but the lease side closed the connection; the port lives inside the lease, not on this operator's loopback", containerPort, lease.LeaseID, b.hostScope(), localPort)
}

func isReadTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// startForwardOwner spawns the detached owner and waits for the record it
// publishes only after its forward is proven. The owner chooses the
// operator-side port after it establishes the lease tunnel.
func (b *backend) startForwardOwner(ctx context.Context, lease core.LeaseTarget, containerPort int, spec string) (publishedForward, error) {
	recordPath, err := forwardRecordPath(lease.LeaseID, containerPort)
	if err != nil {
		return publishedForward{}, err
	}
	logPath, err := forwardLogPath(lease.LeaseID, containerPort)
	if err != nil {
		return publishedForward{}, err
	}
	if err := os.MkdirAll(filepath.Dir(recordPath), 0o700); err != nil {
		return publishedForward{}, core.Exit(5, "create the forward state directory: %v", err)
	}
	spawn := b.spawnOwner
	if spawn == nil {
		spawn = b.spawnForwardOwnerProcess
	}
	pid, exited, err := spawn(spec, logPath)
	if err != nil {
		return publishedForward{}, err
	}
	waitCtx, cancel := context.WithTimeout(ctx, forwardOwnerStartupTimeout)
	defer cancel()
	result, err := shared.Poll(waitCtx, 0, forwardOwnerPollInterval, shared.SleepContext,
		func(context.Context) (publishedForward, error) {
			select {
			case exitErr := <-exited:
				return publishedForward{}, &forwardOwnerExitError{err: exitErr}
			default:
			}
			record, ok, err := readForwardRecord(recordPath)
			if err != nil {
				return publishedForward{}, err
			}
			if !ok {
				return publishedForward{}, errForwardOwnerPending
			}
			return record, nil
		},
		func(_ context.Context, _ publishedForward, err error) (bool, error) {
			if errors.Is(err, errForwardOwnerPending) {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			return true, nil
		},
		nil,
	)
	if err != nil {
		if stopErr := b.signalForwardOwner(pid); stopErr != nil {
			err = errors.Join(err, stopErr)
		}
		return publishedForward{}, b.forwardOwnerFailure(lease.LeaseID, containerPort, logPath, err)
	}
	return result.Value, nil
}

func (b *backend) spawnForwardOwnerProcess(spec, logPath string) (int, <-chan error, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, nil, core.Exit(2, "resolve the crabbox executable for the forward owner: %v", err)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, nil, core.Exit(2, "open the forward owner log %s: %v", logPath, err)
	}
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Env = withEnv(os.Environ(), map[string]string{
		core.PortsForwardOwnerSpecEnv: spec,
	})
	cmd.Stdout, cmd.Stderr = logFile, logFile
	detachForwardOwner(cmd)
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return 0, nil, core.Exit(5, "start the forward owner: %v", err)
	}
	_ = logFile.Close()
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	return cmd.Process.Pid, exited, nil
}

func (b *backend) signalForwardOwner(pid int) error {
	stop := b.stopOwner
	if stop == nil {
		stop = signalForwardOwnerProcess
	}
	return stop(pid)
}

func (b *backend) forwardOwnerFailure(leaseID string, containerPort int, logPath string, cause error) error {
	detail := readLogTail(logPath, forwardLogTailBytes)
	if detail == "" {
		var exit *forwardOwnerExitError
		if errors.As(cause, &exit) {
			detail = exit.Error()
		}
	}
	if detail != "" {
		return core.Exit(5, "publish container port %d on lease %s failed: %s", containerPort, leaseID, detail)
	}
	return core.Exit(5, "publish container port %d on lease %s failed: %v", containerPort, leaseID, cause)
}

// forwardReownedError reports that a different owner took the same container
// port while the previous one was being stopped. Its record must be left alone.
type forwardReownedError struct{ pid int }

func (e *forwardReownedError) Error() string {
	return fmt.Sprintf("the forward was re-owned by pid %d", e.pid)
}

type forwardLockAttempt struct{ release func() }

// waitForwardLock waits until it can take the owner lock, which proves the
// recorded owner is gone. If the record changes to another owner first, it
// reports forwardReownedError instead of waiting for that owner to exit.
func (b *backend) waitForwardLock(ctx context.Context, lockPath string, ownerPID int) (func(), error) {
	recordPath := strings.TrimSuffix(lockPath, ".lock") + ".json"
	waitCtx, cancel := context.WithTimeout(ctx, forwardOwnerStopTimeout)
	defer cancel()
	result, err := shared.Poll(waitCtx, 0, forwardOwnerPollInterval, shared.SleepContext,
		func(context.Context) (forwardLockAttempt, error) {
			release, held, err := tryForwardLock(lockPath, true)
			if err != nil {
				return forwardLockAttempt{}, err
			}
			if !held {
				return forwardLockAttempt{release: release}, nil
			}
			current, ok, err := readForwardRecord(recordPath)
			if err != nil {
				return forwardLockAttempt{}, err
			}
			if ok && current.OwnerPID != ownerPID {
				return forwardLockAttempt{}, &forwardReownedError{pid: current.OwnerPID}
			}
			return forwardLockAttempt{}, nil
		},
		func(_ context.Context, attempt forwardLockAttempt, err error) (bool, error) {
			if err != nil {
				return false, err
			}
			return attempt.release != nil, nil
		},
		nil,
	)
	if err != nil {
		return nil, err
	}
	if result.Value.release == nil {
		return nil, core.Exit(5, "the owner lock %s was never released", lockPath)
	}
	return result.Value.release, nil
}

// stopPublishedForwardOwner stops the owner the record names and removes its
// state while holding the owner lock, so a concurrent publisher cannot be
// orphaned by the removal. Both --unpublish and release go through here.
func (b *backend) stopPublishedForwardOwner(ctx context.Context, leaseID string, record publishedForward) error {
	recordPath, err := forwardRecordPath(leaseID, record.ContainerPort)
	if err != nil {
		return err
	}
	lockPath := forwardLockPathFor(recordPath)
	// create: the lock file is the stable rendezvous; acquiring it here also
	// covers the case where no owner ever created it.
	release, held, err := tryForwardLock(lockPath, true)
	if err != nil {
		return err
	}
	if held {
		if err := b.signalForwardOwner(record.OwnerPID); err != nil {
			return core.Exit(5, "stop the forward owner pid %d for container port %d: %v", record.OwnerPID, record.ContainerPort, err)
		}
		release, err = b.waitForwardLock(ctx, lockPath, record.OwnerPID)
		if err != nil {
			var reowned *forwardReownedError
			if errors.As(err, &reowned) {
				return core.Exit(2, "the forward for container port %d was re-owned by pid %d before it could be stopped; its record is retained", record.ContainerPort, reowned.pid)
			}
			return core.Exit(5, "forward owner pid %d for container port %d did not stop: %v", record.OwnerPID, record.ContainerPort, err)
		}
	}
	defer release()
	// Holding the lock proves no owner is alive, so the record cannot be
	// replaced while it is inspected and removed.
	current, ok, err := readForwardRecord(recordPath)
	if err != nil {
		return err
	}
	if ok && current.OwnerPID != record.OwnerPID {
		return core.Exit(2, "the forward for container port %d was re-owned by pid %d before it could be stopped; its record is retained", record.ContainerPort, current.OwnerPID)
	}
	return removeForwardState(leaseID, record.ContainerPort)
}

// livePublishedForward returns the record only when its owner still holds the
// lock. A record whose lock is free is a leftover from a dead owner and is
// removed here, so a stale pid can never be reported or signalled later.
func (b *backend) livePublishedForward(leaseID string, containerPort int) (publishedForward, bool, error) {
	recordPath, err := forwardRecordPath(leaseID, containerPort)
	if err != nil {
		return publishedForward{}, false, err
	}
	return liveForwardRecord(recordPath)
}

func liveForwardRecord(recordPath string) (publishedForward, bool, error) {
	record, ok, err := readForwardRecord(recordPath)
	if err != nil || !ok {
		return publishedForward{}, false, err
	}
	release, held, err := tryForwardLock(forwardLockPathFor(recordPath), true)
	if err != nil {
		return publishedForward{}, false, err
	}
	if held {
		return record, true, nil
	}
	// Holding the lock proves no owner is alive, and no publisher can write a
	// replacement while it is held.
	defer release()
	_ = os.Remove(recordPath)
	_ = os.Remove(logPathForRecord(recordPath))
	return publishedForward{}, false, nil
}

func (b *backend) livePublishedForwards(leaseID string) ([]publishedForward, error) {
	forwards, err := b.allPublishedForwards(leaseID)
	if err != nil {
		return nil, err
	}
	live := make([]publishedForward, 0, len(forwards))
	for _, forward := range forwards {
		recordPath, err := forwardRecordPath(leaseID, forward.ContainerPort)
		if err != nil {
			return nil, err
		}
		if record, ok, err := liveForwardRecord(recordPath); err != nil {
			return nil, err
		} else if ok {
			live = append(live, record)
		}
	}
	return live, nil
}

func (b *backend) allPublishedForwards(leaseID string) ([]publishedForward, error) {
	dir, err := forwardLeaseDir(leaseID)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, core.Exit(5, "read the forward state directory %s: %v", dir, err)
	}
	forwards := make([]publishedForward, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		record, ok, err := readForwardRecord(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		if ok {
			forwards = append(forwards, record)
		}
	}
	sort.Slice(forwards, func(i, j int) bool { return forwards[i].ContainerPort < forwards[j].ContainerPort })
	return forwards, nil
}

// reapPublishedForwards stops every owner a lease still has. Release calls it
// before the container: a leftover owner would forward to a dead container.
func (b *backend) reapPublishedForwards(ctx context.Context, leaseID string) error {
	// A synthetic/non-canonical id cannot own forward state (records are only
	// written after Resolve returns a canonical id), and the release path must
	// not fail on it.
	if !core.IsCanonicalLeaseID(leaseID) {
		return nil
	}
	forwards, err := b.allPublishedForwards(leaseID)
	if err != nil {
		return err
	}
	for _, forward := range forwards {
		if err := b.stopPublishedForwardOwner(ctx, leaseID, forward); err != nil {
			return err
		}
	}
	return nil
}

// removeForwardState deletes a stopped forward's record, lock and log, and the
// lease directory when it holds no other forward. It is only called once the
// owner is gone, never while the lock still proves one is alive.
// removeForwardState deletes a stopped forward's record and log. The lock file
// is deliberately kept: it is the stable rendezvous publishers and stopppers
// use, and unlinking it would let a concurrent publisher lock a new inode while
// another process still holds the old one (two owners for one container port).
func removeForwardState(leaseID string, containerPort int) error {
	recordPath, err := forwardRecordPath(leaseID, containerPort)
	if err != nil {
		return err
	}
	for _, path := range []string{recordPath, logPathForRecord(recordPath)} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return core.Exit(5, "remove forward state %s: %v", path, err)
		}
	}
	return nil
}

// parsePublishSpec accepts [LOCAL_PORT:]CONTAINER_PORT. The bind is always the
// operator's IPv4 loopback; a host-IP form is refused rather than silently
// ignored.
func parsePublishSpec(spec string) (containerPort, localPort int, err error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return 0, 0, core.Exit(2, "empty port mapping; use [LOCAL_PORT:]CONTAINER_PORT")
	}
	parts := strings.Split(spec, ":")
	if len(parts) > 2 {
		return 0, 0, core.Exit(2, "containerd publishes loopback forwards only; use [LOCAL_PORT:]CONTAINER_PORT, not %q", spec)
	}
	containerPort, err = parsePublishPort(parts[len(parts)-1])
	if err != nil {
		return 0, 0, err
	}
	if len(parts) == 2 {
		localPort, err = parsePublishPort(parts[0])
		if err != nil {
			return 0, 0, err
		}
	}
	return containerPort, localPort, nil
}

func parsePublishPort(value string) (int, error) {
	port, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || port < 1 || port > 65535 {
		return 0, core.Exit(2, "port %q must be a TCP port in 1..65535", value)
	}
	return port, nil
}

func forwardMapping(record publishedForward) portMapping {
	return portMapping{
		Source:        "forward",
		LocalHost:     record.Host,
		LocalPort:     record.LocalPort,
		ContainerPort: record.ContainerPort,
		OwnerPID:      record.OwnerPID,
		HostScope:     record.HostScope,
		LeaseID:       record.LeaseID,
	}
}

func portMappingText(mapping portMapping) string {
	if mapping.Source == "nerdctl" {
		return mapping.Mapping
	}
	return fmt.Sprintf("forward %s:%d -> container:%d (lease %s, pid %d, host %s)",
		mapping.LocalHost, mapping.LocalPort, mapping.ContainerPort, mapping.LeaseID, mapping.OwnerPID, mapping.HostScope)
}

func renderPortMappings(jsonOut bool, mappings []portMapping) (string, error) {
	if jsonOut {
		return mustJSON(mappings), nil
	}
	lines := make([]string, 0, len(mappings))
	for _, mapping := range mappings {
		lines = append(lines, portMappingText(mapping))
	}
	return strings.Join(lines, "\n"), nil
}

func forwardLeaseDir(leaseID string) (string, error) {
	state, err := core.CrabboxStateDir()
	if err != nil {
		return "", err
	}
	leaseID = strings.TrimSpace(leaseID)
	if !core.IsCanonicalLeaseID(leaseID) {
		return "", core.Exit(5, "refusing to build a forward state path for non-canonical lease %q", leaseID)
	}
	return filepath.Join(state, forwardStateDirName, leaseID), nil
}

func forwardRecordPath(leaseID string, containerPort int) (string, error) {
	dir, err := forwardLeaseDir(leaseID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, strconv.Itoa(containerPort)+".json"), nil
}

func forwardLockPath(leaseID string, containerPort int) (string, error) {
	recordPath, err := forwardRecordPath(leaseID, containerPort)
	if err != nil {
		return "", err
	}
	return forwardLockPathFor(recordPath), nil
}

func forwardLockPathFor(recordPath string) string {
	return strings.TrimSuffix(recordPath, ".json") + ".lock"
}

func logPathForRecord(recordPath string) string {
	return strings.TrimSuffix(recordPath, ".json") + ".log"
}

func forwardLogPath(leaseID string, containerPort int) (string, error) {
	recordPath, err := forwardRecordPath(leaseID, containerPort)
	if err != nil {
		return "", err
	}
	return logPathForRecord(recordPath), nil
}

func readForwardRecord(path string) (publishedForward, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return publishedForward{}, false, nil
	}
	if err != nil {
		return publishedForward{}, false, core.Exit(5, "read the forward record %s: %v", path, err)
	}
	var record publishedForward
	if err := json.Unmarshal(data, &record); err != nil {
		return publishedForward{}, false, core.Exit(5, "the forward record %s is unreadable: %v", path, err)
	}
	return record, true, nil
}

func writeForwardRecord(path string, record publishedForward) error {
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return core.Exit(5, "encode the forward record: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return core.Exit(5, "create the forward state directory: %v", err)
	}
	if err := atomicfile.WritePrivate(path, "."+filepath.Base(path)+".tmp-*", data, os.Rename); err != nil {
		return core.Exit(5, "write the forward record %s: %v", path, err)
	}
	return nil
}

func readLogTail(path string, limit int64) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	if info, err := file.Stat(); err == nil && info.Size() > limit {
		if _, err := file.Seek(-limit, io.SeekEnd); err != nil {
			return ""
		}
	}
	data, err := io.ReadAll(io.LimitReader(file, limit))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// withEnv replaces every named variable instead of appending a duplicate: a
// duplicate key is resolved inconsistently (some libc getenv implementations
// return the FIRST match), so the owner could inherit the parent's values.
func withEnv(base []string, overrides map[string]string) []string {
	out := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		key := entry
		if index := strings.IndexByte(entry, '='); index >= 0 {
			key = entry[:index]
		}
		if _, replaced := overrides[key]; replaced {
			continue
		}
		out = append(out, entry)
	}
	for key, value := range overrides {
		out = append(out, key+"="+value)
	}
	return out
}
