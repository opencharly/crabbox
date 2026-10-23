package containerd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared/containercheckpoint"
)

const (
	checkpointTestImageID     = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	checkpointTestNewImageID  = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	checkpointTestColdImageID = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
	checkpointTestContainer   = "crabbox-blue-lobster-00000000"
	checkpointTestLeaseID     = "cbx_0123456789ab"
	checkpointTestInputsDir   = "/home/operator/.local/state/crabbox/containerd/leases/cbx_0123456789ab/inputs"
	checkpointTestStateVolume = "crabbox-cbx_0123456789ab-state"
	checkpointTestDaemonID    = "daemon-id"
)

// useCheckpointRuntime installs a recording executor for the provider-operation
// path and restores the real one after the test.
func useCheckpointRuntime(t *testing.T, runner *recordingRunner) {
	t.Helper()
	previous := providerOperationRuntime
	providerOperationRuntime = func(io.Writer) core.Runtime {
		return core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}
	}
	t.Cleanup(func() { providerOperationRuntime = previous })
}

func useCheckpointSSH(t *testing.T) {
	t.Helper()
	previous := waitForCheckpointSSH
	waitForCheckpointSSH = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error { return nil }
	t.Cleanup(func() { waitForCheckpointSSH = previous })
}

func checkpointTestConfig() core.Config {
	cfg := testBackend(&recordingRunner{}).cfg
	cfg.Provider = providerName
	return cfg
}

func checkpointTestMetadata(cfg core.Config) map[string]string {
	return map[string]string{
		checkpointMetadataRuntime:        cfg.Containerd.Runtime,
		checkpointMetadataRuntimePath:    "/usr/bin/crun",
		checkpointMetadataRootless:       "false",
		checkpointMetadataNamespace:      cfg.Containerd.Namespace,
		checkpointMetadataSnapshotter:    cfg.Containerd.Snapshotter,
		checkpointMetadataAddress:        cfg.Containerd.Address,
		checkpointMetadataHost:           "local",
		checkpointMetadataDaemon:         checkpointTestDaemonID,
		checkpointMetadataContainer:      checkpointTestContainer,
		checkpointMetadataLease:          checkpointTestLeaseID,
		checkpointMetadataStateVolume:    checkpointTestStateVolume,
		checkpointMetadataCheckpointVol:  "crabbox-cp-cp-0123456789ab-state",
		checkpointMetadataImage:          cfg.Containerd.Image,
		checkpointMetadataSSHUser:        cfg.Containerd.User,
		checkpointMetadataWorkRoot:       cfg.Containerd.WorkRoot,
		checkpointMetadataInit:           cfg.Containerd.InitMode,
		checkpointMetadataNested:         cfg.Containerd.Nested,
		checkpointMetadataGPU:            cfg.Containerd.GPU,
		checkpointMetadataSeccomp:        cfg.Containerd.SeccompProfile,
		checkpointMetadataCPUs:           "0",
		checkpointMetadataMemory:         "",
		checkpointMetadataKrunGPUFlags:   "962",
		checkpointMetadataKrunHeadroom:   "256",
		checkpointMetadataKrunNestedVirt: "false",
		checkpointMetadataKrunNetwork:    cfg.Containerd.KrunNetwork,
		checkpointMetadataKrunKernel:     cfg.Containerd.KrunKernel,
		checkpointMetadataDesktop:        "false",
		checkpointMetadataDesktopEnv:     "xfce",
		checkpointMetadataBrowser:        "false",
		checkpointMetadataNetwork:        cfg.Containerd.Network,
		checkpointMetadataInputsDir:      checkpointTestInputsDir,
		checkpointMetadataPublishedPort:  "55283",
		checkpointMetadataExtraPorts:     `["127.0.0.1:55284:8080"]`,
	}
}

func checkpointTestServer() core.Server {
	return core.Server{
		Name:    checkpointTestContainer,
		CloudID: "container-id",
		Labels: map[string]string{
			"slug":           "blue-lobster",
			"lease":          checkpointTestLeaseID,
			"state_volume":   checkpointTestStateVolume,
			"inputs_dir":     checkpointTestInputsDir,
			"published_port": "55283",
			"state":          "ready",
		},
	}
}

func checkpointTestTarget() core.SSHTarget {
	return core.SSHTarget{Host: "127.0.0.1", Port: "55283", User: "crabbox", TargetOS: core.TargetLinux}
}

// checkpointBaseRunner answers the daemon and scope probes every checkpoint
// operation starts with.
func checkpointBaseRunner(t *testing.T, run func(core.LocalCommandRequest) (core.LocalCommandResult, error)) *recordingRunner {
	t.Helper()
	return &recordingRunner{run: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		switch {
		case req.Name == "nerdctl" && findArg(req.Args, "info"):
			output, err := jsonInfo(daemonInfo{ID: checkpointTestDaemonID})
			return core.LocalCommandResult{Stdout: output}, err
		case req.Name == "nerdctl" && findArg(req.Args, "namespace", "ls"):
			return core.LocalCommandResult{Stdout: "default\n"}, nil
		}
		if run != nil {
			return run(req)
		}
		return core.LocalCommandResult{}, nil
	}}
}

func callIndex(runner *recordingRunner, match func(core.LocalCommandRequest) bool) int {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	for index, call := range runner.calls {
		if match(call) {
			return index
		}
	}
	return -1
}

func callWith(runner *recordingRunner, match func(core.LocalCommandRequest) bool) (core.LocalCommandRequest, bool) {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	for _, call := range runner.calls {
		if match(call) {
			return call, true
		}
	}
	return core.LocalCommandRequest{}, false
}

func isGuestSSH(req core.LocalCommandRequest) bool {
	return req.Name == "ssh"
}

func guestScript(req core.LocalCommandRequest) string {
	if !isGuestSSH(req) {
		return ""
	}
	return strings.Join(req.Args, " ")
}

// TestCreateNativeCheckpointSequence pins the whole capture order: GNU cp,
// machine-id save+empty, pause, commit, state-volume clone, unpause, restore.
func TestCreateNativeCheckpointSequence(t *testing.T) {
	runner := checkpointBaseRunner(t, func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		switch {
		case isGuestSSH(req) && strings.Contains(guestScript(req), "cp --version"):
			return core.LocalCommandResult{Stdout: "cp (GNU coreutils) 9.4\n"}, nil
		case isGuestSSH(req) && strings.Contains(guestScript(req), ": > /etc/machine-id"):
			return core.LocalCommandResult{Stdout: "original-machine-id\n"}, nil
		case isGuestSSH(req) && strings.Contains(guestScript(req), "cat > /etc/machine-id"):
			return core.LocalCommandResult{}, nil
		case findArg(req.Args, "inspect", checkpointTestContainer, "--format", "{{.State.Status}}"):
			return core.LocalCommandResult{Stdout: "running\n"}, nil
		case findArg(req.Args, "port", checkpointTestContainer):
			return core.LocalCommandResult{Stdout: "2222/tcp -> 127.0.0.1:55283\n8080/tcp -> 127.0.0.1:55284\n"}, nil
		case findArg(req.Args, "pause", checkpointTestContainer):
			return core.LocalCommandResult{}, nil
		case findArg(req.Args, "unpause", checkpointTestContainer):
			return core.LocalCommandResult{}, nil
		case findArg(req.Args, "commit"):
			return core.LocalCommandResult{Stdout: checkpointTestNewImageID + "\n"}, nil
		case findArg(req.Args, "run", "--rm") && findArg(req.Args, "-v", checkpointTestStateVolume+":/src:ro"):
			return core.LocalCommandResult{}, nil
		}
		return core.LocalCommandResult{}, nil
	})
	useCheckpointRuntime(t, runner)
	cfg := checkpointTestConfig()
	req := core.NativeCheckpointCreateRequest{
		Config: cfg, Server: checkpointTestServer(), Target: checkpointTestTarget(),
		CheckpointID: "cp-0123456789ab", LeaseID: checkpointTestLeaseID, Name: "checkpoint", RepoName: "example",
	}
	result, err := (Provider{}).CreateNativeCheckpoint(t.Context(), req)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if result.Image.ID != checkpointTestNewImageID || result.Image.Kind != core.CheckpointKindContainerdCommit || !result.CaptureEstablished {
		t.Fatalf("image=%+v captureEstablished=%v", result.Image, result.CaptureEstablished)
	}

	pause := callIndex(runner, func(req core.LocalCommandRequest) bool { return findArg(req.Args, "pause", checkpointTestContainer) })
	commit := callIndex(runner, func(req core.LocalCommandRequest) bool { return findArg(req.Args, "commit") })
	copyVolume := callIndex(runner, func(req core.LocalCommandRequest) bool {
		return findArg(req.Args, "run", "--rm") && findArg(req.Args, "-v", checkpointTestStateVolume+":/src:ro")
	})
	unpause := callIndex(runner, func(req core.LocalCommandRequest) bool { return findArg(req.Args, "unpause", checkpointTestContainer) })
	restore := callIndex(runner, func(req core.LocalCommandRequest) bool {
		return isGuestSSH(req) && strings.Contains(guestScript(req), "cat > /etc/machine-id")
	})
	for name, index := range map[string]int{"pause": pause, "commit": commit, "copy": copyVolume, "unpause": unpause, "restore": restore} {
		if index < 0 {
			t.Fatalf("%s did not run", name)
		}
	}
	if !(pause < commit && commit < copyVolume && copyVolume < unpause && unpause < restore) {
		t.Fatalf("capture order pause=%d commit=%d copy=%d unpause=%d restore=%d", pause, commit, copyVolume, unpause, restore)
	}

	commitCall, _ := callWith(runner, func(req core.LocalCommandRequest) bool { return findArg(req.Args, "commit") })
	for _, arg := range commitCall.Args {
		if strings.Contains(strings.ToUpper(arg), "LABEL") {
			t.Fatalf("nerdctl commit --change must not carry a LABEL directive: %q", arg)
		}
	}
	if !findArg(commitCall.Args, "--change", containercheckpoint.BootableCommandChange()) {
		t.Fatalf("commit argv=%v missing the bootable CMD change", commitCall.Args)
	}
	if !findArg(commitCall.Args, "--pause=false") {
		t.Fatalf("commit argv=%v must disable the implicit pause", commitCall.Args)
	}
	copyCall, _ := callWith(runner, func(req core.LocalCommandRequest) bool {
		return findArg(req.Args, "run", "--rm") && findArg(req.Args, "-v", checkpointTestStateVolume+":/src:ro")
	})
	for _, want := range [][]string{
		{"-v", checkpointTestStateVolume + ":/src:ro"},
		{"-v", "crabbox-cp-cp-0123456789ab-state:/dst"},
		{"--entrypoint", "/bin/sh"},
		{"--runtime", "/usr/bin/crun"},
		{"--privileged"},
	} {
		if !findArg(copyCall.Args, want...) {
			t.Fatalf("copy helper argv=%v missing %v", copyCall.Args, want)
		}
	}
	// The copy uses the image that was just committed, BY TAG: a bare
	// "sha256:<hex>" is not a valid nerdctl reference (MEASURED: it parses as
	// host:port and fails 'invalid port ... after host'), so the digest the
	// commit returned cannot be the helper's image argument. No guest command
	// ever reaches for nerdctl exec or --pid host.
	committedTag := containercheckpoint.TagName("checkpoint", "cp-0123456789ab")
	if !findArg(copyCall.Args, committedTag) {
		t.Fatalf("copy helper argv=%v did not use the committed tag %q", copyCall.Args, committedTag)
	}
	runner.mu.Lock()
	for _, call := range runner.calls {
		joined := strings.Join(call.Args, " ")
		if strings.Contains(joined, "exec") || strings.Contains(joined, "--pid") {
			runner.mu.Unlock()
			t.Fatalf("checkpoint sequence used a forbidden invocation: %s", joined)
		}
	}
	runner.mu.Unlock()

	restoreCall, _ := callWith(runner, func(req core.LocalCommandRequest) bool {
		return isGuestSSH(req) && strings.Contains(guestScript(req), "cat > /etc/machine-id")
	})
	body, _ := io.ReadAll(restoreCall.Stdin)
	if string(body) != "original-machine-id\n" {
		t.Fatalf("restored machine-id=%q", string(body))
	}
	if result.Metadata[checkpointMetadataPublishedPort] != "55283" ||
		result.Metadata[checkpointMetadataExtraPorts] != `["127.0.0.1:55284:8080"]` ||
		result.Metadata[checkpointMetadataCheckpointVol] != "crabbox-cp-cp-0123456789ab-state" ||
		result.Metadata[checkpointMetadataCold] != "false" {
		t.Fatalf("metadata=%v", result.Metadata)
	}
}

// A busybox cp silently drops trusted.overlay.* xattrs, so the capture must
// refuse BEFORE pausing or committing anything.
func TestCreateNativeCheckpointRefusesBusyboxCopyBeforeCapture(t *testing.T) {
	runner := checkpointBaseRunner(t, func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		switch {
		case isGuestSSH(req) && strings.Contains(guestScript(req), "cp --version"):
			return core.LocalCommandResult{ExitCode: 1, Stderr: "BusyBox v1.36.1 multi-call binary\n"}, errors.New("exit status 1")
		case findArg(req.Args, "inspect", checkpointTestContainer, "--format", "{{.State.Status}}"):
			return core.LocalCommandResult{Stdout: "running\n"}, nil
		}
		return core.LocalCommandResult{}, nil
	})
	useCheckpointRuntime(t, runner)
	_, err := (Provider{}).CreateNativeCheckpoint(t.Context(), core.NativeCheckpointCreateRequest{
		Config: checkpointTestConfig(), Server: checkpointTestServer(), Target: checkpointTestTarget(),
		CheckpointID: "cp-0123456789ab", LeaseID: checkpointTestLeaseID, Name: "checkpoint",
	})
	if err == nil || !strings.Contains(err.Error(), "busybox") || !strings.Contains(err.Error(), "trusted.overlay") {
		t.Fatalf("err=%v, want the busybox refusal", err)
	}
	for _, forbidden := range [][]string{{"pause"}, {"commit"}, {"run"}} {
		if hasRecordedCall(runner.calls, forbidden...) {
			t.Fatalf("the busybox refusal ran %v before refusing", forbidden)
		}
	}
}

// The machine-id restore is a finally step: a failed commit must not leave the
// live lease with an empty machine-id or paused.
func TestCreateNativeCheckpointRestoresMachineIDWhenTheCommitFails(t *testing.T) {
	runner := checkpointBaseRunner(t, func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		switch {
		case isGuestSSH(req) && strings.Contains(guestScript(req), "cp --version"):
			return core.LocalCommandResult{Stdout: "cp (GNU coreutils) 9.4\n"}, nil
		case isGuestSSH(req) && strings.Contains(guestScript(req), ": > /etc/machine-id"):
			return core.LocalCommandResult{Stdout: "original-machine-id\n"}, nil
		case isGuestSSH(req) && strings.Contains(guestScript(req), "cat > /etc/machine-id"):
			return core.LocalCommandResult{}, nil
		case findArg(req.Args, "inspect", checkpointTestContainer, "--format", "{{.State.Status}}"):
			return core.LocalCommandResult{Stdout: "running\n"}, nil
		case findArg(req.Args, "port", checkpointTestContainer):
			return core.LocalCommandResult{Stdout: "2222/tcp -> 127.0.0.1:55283\n"}, nil
		case findArg(req.Args, "pause", checkpointTestContainer), findArg(req.Args, "unpause", checkpointTestContainer):
			return core.LocalCommandResult{}, nil
		case findArg(req.Args, "commit"):
			return core.LocalCommandResult{ExitCode: 1, Stderr: "commit exploded"}, errors.New("exit status 1")
		}
		return core.LocalCommandResult{}, nil
	})
	useCheckpointRuntime(t, runner)
	_, err := (Provider{}).CreateNativeCheckpoint(t.Context(), core.NativeCheckpointCreateRequest{
		Config: checkpointTestConfig(), Server: checkpointTestServer(), Target: checkpointTestTarget(),
		CheckpointID: "cp-0123456789ab", LeaseID: checkpointTestLeaseID, Name: "checkpoint",
	})
	if err == nil || !strings.Contains(err.Error(), "commit exploded") {
		t.Fatalf("err=%v, want the commit failure", err)
	}
	if !hasRecordedCall(runner.calls, "unpause", checkpointTestContainer) {
		t.Fatal("a failed commit left the lease paused")
	}
	restoreCall, ok := callWith(runner, func(req core.LocalCommandRequest) bool {
		return isGuestSSH(req) && strings.Contains(guestScript(req), "cat > /etc/machine-id")
	})
	if !ok {
		t.Fatal("a failed commit skipped the machine-id restore")
	}
	body, _ := io.ReadAll(restoreCall.Stdin)
	if string(body) != "original-machine-id\n" {
		t.Fatalf("restored machine-id=%q", string(body))
	}
}

// A stopped source is a cold checkpoint: no pause, no SSH machine-id step, and
// the empty machine-id is produced in a derived image instead.
func TestCreateNativeCheckpointColdSourceDerivesTheMachineID(t *testing.T) {
	runner := checkpointBaseRunner(t, func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		switch {
		case findArg(req.Args, "inspect", checkpointTestContainer, "--format", "{{.State.Status}}"):
			return core.LocalCommandResult{Stdout: "exited\n"}, nil
		case findArg(req.Args, "port", checkpointTestContainer):
			return core.LocalCommandResult{Stdout: "2222/tcp -> 127.0.0.1:55283\n"}, nil
		case findArg(req.Args, "run", "--rm") && findArg(req.Args, "-c", "cp --version 2>&1"):
			return core.LocalCommandResult{Stdout: "cp (GNU coreutils) 9.4\n"}, nil
		case findArg(req.Args, "commit"):
			for _, arg := range req.Args {
				if strings.HasSuffix(arg, "cp-0123456789ab-source") {
					return core.LocalCommandResult{Stdout: checkpointTestColdImageID + "\n"}, nil
				}
			}
			return core.LocalCommandResult{Stdout: checkpointTestNewImageID + "\n"}, nil
		case findArg(req.Args, "run", "--name") && findArg(req.Args, "-c", ": > /etc/machine-id"):
			return core.LocalCommandResult{}, nil
		case findArg(req.Args, "run", "--rm") && findArg(req.Args, "-v", checkpointTestStateVolume+":/src:ro"):
			return core.LocalCommandResult{}, nil
		}
		return core.LocalCommandResult{}, nil
	})
	useCheckpointRuntime(t, runner)
	result, err := (Provider{}).CreateNativeCheckpoint(t.Context(), core.NativeCheckpointCreateRequest{
		Config: checkpointTestConfig(), Server: checkpointTestServer(), Target: checkpointTestTarget(),
		CheckpointID: "cp-0123456789ab", LeaseID: checkpointTestLeaseID, Name: "checkpoint",
	})
	if err != nil {
		t.Fatalf("cold create: %v", err)
	}
	if result.Image.ID != checkpointTestNewImageID || result.Metadata[checkpointMetadataCold] != "true" {
		t.Fatalf("image=%+v metadata=%v", result.Image, result.Metadata)
	}
	if hasRecordedCall(runner.calls, "pause") || hasRecordedCall(runner.calls, "unpause") {
		t.Fatal("a cold checkpoint paused or unpaused the stopped source")
	}
	if callIndex(runner, func(req core.LocalCommandRequest) bool { return isGuestSSH(req) }) >= 0 {
		t.Fatal("a cold checkpoint reached the stopped source over SSH")
	}
	commits := 0
	runner.mu.Lock()
	for _, call := range runner.calls {
		if findArg(call.Args, "commit") {
			commits++
		}
	}
	runner.mu.Unlock()
	if commits != 2 {
		t.Fatalf("cold capture used %d commits, want the source and derived commits", commits)
	}
	if !hasRecordedCall(runner.calls, "rm", "-f", "crabbox-checkpoint-checkpoint-cp-0123456789ab-mid") {
		t.Fatal("cold capture left its machine-id helper container behind")
	}
}

// Verify/delete must fail closed when the recorded daemon scope no longer
// matches the daemon this process reaches: exit 7, before any mutation.
func TestCheckpointScopeDriftIsExit7(t *testing.T) {
	runner := checkpointBaseRunner(t, nil)
	useCheckpointRuntime(t, runner)
	metadata := checkpointTestMetadata(checkpointTestConfig())
	metadata[checkpointMetadataDaemon] = "other-daemon"
	_, err := (Provider{}).VerifyNativeCheckpoint(t.Context(), core.NativeCheckpointResourceRequest{
		LoadConfig: func() (core.Config, error) { return checkpointTestConfig(), nil },
		Image:      core.NativeCheckpointImage{ID: checkpointTestNewImageID, Name: "crabbox-checkpoint-checkpoint-cp-0123456789ab"},
		Metadata:   metadata,
	})
	var failure core.ExitError
	if !core.AsExitError(err, &failure) || failure.Code != 7 {
		t.Fatalf("err=%v, want exit 7 scope drift", err)
	}
	if hasRecordedCall(runner.calls, "image", "inspect") {
		t.Fatal("scope drift was detected after inspecting the image")
	}
}

func TestVerifyNativeCheckpointReportsMissingAndConflict(t *testing.T) {
	for _, tc := range []struct {
		name       string
		inspect    func() (core.LocalCommandResult, error)
		wantState  string
		wantAction string
	}{
		{
			name: "missing",
			inspect: func() (core.LocalCommandResult, error) {
				return core.LocalCommandResult{ExitCode: 1, Stderr: "no such image"}, errors.New("exit status 1")
			},
			wantState:  "missing",
			wantAction: "delete_local",
		},
		{
			name: "conflict",
			inspect: func() (core.LocalCommandResult, error) {
				return core.LocalCommandResult{Stdout: checkpointTestImageID}, nil
			},
			wantState:  "conflict",
			wantAction: "restore_tag_or_delete_local",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := checkpointBaseRunner(t, func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
				if findArg(req.Args, "image", "inspect") {
					return tc.inspect()
				}
				return core.LocalCommandResult{}, nil
			})
			useCheckpointRuntime(t, runner)
			got, err := (Provider{}).VerifyNativeCheckpoint(t.Context(), core.NativeCheckpointResourceRequest{
				LoadConfig: func() (core.Config, error) { return checkpointTestConfig(), nil },
				Image:      core.NativeCheckpointImage{ID: checkpointTestNewImageID, Name: "crabbox-checkpoint-checkpoint-cp-0123456789ab"},
				Metadata:   checkpointTestMetadata(checkpointTestConfig()),
			})
			if err != nil {
				t.Fatalf("verify: %v", err)
			}
			if got.ProviderState != tc.wantState || got.NextAction != tc.wantAction {
				t.Fatalf("verify=%+v want state=%s action=%s", got, tc.wantState, tc.wantAction)
			}
		})
	}
}

func TestDeleteNativeCheckpointRemovesTheTagAndSnapshotVolume(t *testing.T) {
	runner := checkpointBaseRunner(t, func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		switch {
		case findArg(req.Args, "image", "inspect"):
			return core.LocalCommandResult{Stdout: checkpointTestNewImageID}, nil
		case findArg(req.Args, "rmi", "-f"):
			return core.LocalCommandResult{}, nil
		case findArg(req.Args, "volume", "rm", "--force"):
			return core.LocalCommandResult{}, nil
		}
		return core.LocalCommandResult{}, nil
	})
	useCheckpointRuntime(t, runner)
	if err := (Provider{}).DeleteNativeCheckpoint(t.Context(), core.NativeCheckpointResourceRequest{
		LoadConfig: func() (core.Config, error) { return checkpointTestConfig(), nil },
		Image:      core.NativeCheckpointImage{ID: checkpointTestNewImageID, Name: "crabbox-checkpoint-checkpoint-cp-0123456789ab"},
		Metadata:   checkpointTestMetadata(checkpointTestConfig()),
	}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !hasRecordedCall(runner.calls, "rmi", "-f", "crabbox-checkpoint-checkpoint-cp-0123456789ab") {
		t.Fatal("delete did not remove the recorded tag")
	}
	if !hasRecordedCall(runner.calls, "volume", "rm", "--force", "crabbox-cp-cp-0123456789ab-state") {
		t.Fatal("delete did not remove the snapshot volume")
	}
}

func TestDeleteNativeCheckpointRefusesAReplacedTag(t *testing.T) {
	runner := checkpointBaseRunner(t, func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		if findArg(req.Args, "image", "inspect") {
			return core.LocalCommandResult{Stdout: checkpointTestImageID}, nil
		}
		return core.LocalCommandResult{}, nil
	})
	useCheckpointRuntime(t, runner)
	err := (Provider{}).DeleteNativeCheckpoint(t.Context(), core.NativeCheckpointResourceRequest{
		LoadConfig: func() (core.Config, error) { return checkpointTestConfig(), nil },
		Image:      core.NativeCheckpointImage{ID: checkpointTestNewImageID, Name: "crabbox-checkpoint-checkpoint-cp-0123456789ab"},
		Metadata:   checkpointTestMetadata(checkpointTestConfig()),
	})
	var failure core.ExitError
	if !core.AsExitError(err, &failure) || failure.Code != 7 {
		t.Fatalf("err=%v, want exit 7 for a replaced tag", err)
	}
	if hasRecordedCall(runner.calls, "rmi") || hasRecordedCall(runner.calls, "volume", "rm") {
		t.Fatal("a replaced tag was deleted anyway")
	}
}

// A fork replays the recorded image and lease settings; Acquire then clones the
// captured state volume and applies fresh lease labels.
func TestApplyNativeCheckpointForkConfigReplaysTheRecordedScope(t *testing.T) {
	cfg := checkpointTestConfig()
	cfg.Containerd.Runtime = "krun"
	cfg.Containerd.Image = "old-image"
	metadata := checkpointTestMetadata(cfg)
	metadata[checkpointMetadataImage] = "old-image"
	err := (Provider{}).ApplyNativeCheckpointForkConfig(core.NativeCheckpointForkRequest{
		Config: &cfg,
		Record: core.NativeCheckpointForkRecord{
			Kind: core.CheckpointKindContainerdCommit, ImageID: checkpointTestNewImageID,
			Name: "crabbox-checkpoint-checkpoint-cp-0123456789ab", Metadata: metadata,
		},
	})
	if err != nil {
		t.Fatalf("fork config: %v", err)
	}
	if cfg.Containerd.Image != "crabbox-checkpoint-checkpoint-cp-0123456789ab" {
		t.Fatalf("fork image=%q", cfg.Containerd.Image)
	}
	if cfg.Containerd.Runtime != "krun" || cfg.Containerd.Snapshotter != "overlayfs" || cfg.Containerd.Namespace != "default" {
		t.Fatalf("fork scope=%+v", cfg.Containerd)
	}
	if cfg.Containerd.CheckpointMetadata[checkpointMetadataForkImageID] != checkpointTestNewImageID ||
		cfg.Containerd.CheckpointMetadata[checkpointMetadataCheckpointVol] != "crabbox-cp-cp-0123456789ab-state" {
		t.Fatalf("fork metadata=%v", cfg.Containerd.CheckpointMetadata)
	}
}

// Restore keeps the lease id, claim, key and published ports, re-clones the
// state volume, and re-applies the claim's labels to the recreated container.
func TestRestoreNativeCheckpointRecreatesWithTheSamePortsAndState(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	published := fmt.Sprint(listener.Addr().(*net.TCPAddr).Port)
	useCheckpointSSH(t)

	runner := checkpointBaseRunner(t, func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		switch {
		case findArg(req.Args, "image", "inspect"):
			return core.LocalCommandResult{Stdout: checkpointTestNewImageID}, nil
		case findArg(req.Args, "port", checkpointTestContainer):
			return core.LocalCommandResult{Stdout: "2222/tcp -> 127.0.0.1:" + published + "\n8080/tcp -> 127.0.0.1:55284\n"}, nil
		case findArg(req.Args, "volume", "ls", "--format"):
			return core.LocalCommandResult{Stdout: "crabbox-cp-cp-0123456789ab-state\n"}, nil
		case req.Name == "sh" && strings.Contains(strings.Join(req.Args, " "), "test -e "+checkpointTestInputsDir):
			return core.LocalCommandResult{ExitCode: 0}, nil
		case findArg(req.Args, "rm", "-f", checkpointTestContainer):
			return core.LocalCommandResult{}, nil
		case findArg(req.Args, "volume", "rm", "--force", checkpointTestStateVolume):
			return core.LocalCommandResult{}, nil
		case findArg(req.Args, "run", "--rm") && findArg(req.Args, "-v", "crabbox-cp-cp-0123456789ab-state:/src:ro"):
			return core.LocalCommandResult{}, nil
		case findArg(req.Args, "run", "-d", "--name", checkpointTestContainer):
			return core.LocalCommandResult{Stdout: "new-container-id\n"}, nil
		case isGuestSSH(req) && strings.Contains(guestScript(req), "/usr/local/libexec/crabbox/ready"):
			return core.LocalCommandResult{ExitCode: 0}, nil
		}
		return core.LocalCommandResult{}, nil
	})
	useCheckpointRuntime(t, runner)
	metadata := checkpointTestMetadata(checkpointTestConfig())
	metadata[checkpointMetadataPublishedPort] = published
	result, err := (Provider{}).RestoreNativeCheckpoint(t.Context(), core.NativeCheckpointRestoreRequest{
		Config: checkpointTestConfig(), Server: checkpointTestServer(), Target: checkpointTestTarget(),
		LeaseID: checkpointTestLeaseID, RepoName: "example",
		Record: core.NativeCheckpointRestoreRecord{
			Kind: core.CheckpointKindContainerdCommit, ImageID: checkpointTestNewImageID,
			Name: "crabbox-checkpoint-checkpoint-cp-0123456789ab", Metadata: metadata,
		},
	})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !result.Converged || !strings.Contains(result.Message, "readiness=ok") {
		t.Fatalf("restore result=%+v", result)
	}
	remove := callIndex(runner, func(req core.LocalCommandRequest) bool { return findArg(req.Args, "rm", "-f", checkpointTestContainer) })
	clone := callIndex(runner, func(req core.LocalCommandRequest) bool {
		return findArg(req.Args, "run", "--rm") && findArg(req.Args, "-v", "crabbox-cp-cp-0123456789ab-state:/src:ro")
	})
	recreate := callIndex(runner, func(req core.LocalCommandRequest) bool {
		return findArg(req.Args, "run", "-d", "--name", checkpointTestContainer)
	})
	if remove < 0 || clone < 0 || recreate < 0 || !(remove < clone && clone < recreate) {
		t.Fatalf("restore order remove=%d clone=%d recreate=%d", remove, clone, recreate)
	}
	recreateCall, _ := callWith(runner, func(req core.LocalCommandRequest) bool {
		return findArg(req.Args, "run", "-d", "--name", checkpointTestContainer)
	})
	for _, want := range [][]string{
		{"-p", "127.0.0.1:" + published + ":2222"},
		{"-p", "127.0.0.1:55284:8080"},
		{"-v", checkpointTestStateVolume + ":/var"},
		{"--label", "lease=" + checkpointTestLeaseID},
		{"--runtime", "/usr/bin/crun"},
		{"crabbox-checkpoint-checkpoint-cp-0123456789ab"},
	} {
		if !findArg(recreateCall.Args, want...) {
			t.Fatalf("recreated container argv=%v missing %v", recreateCall.Args, want)
		}
	}
	if hasRecordedCall(runner.calls, "rmi") {
		t.Fatal("restore deleted the checkpoint image")
	}
}

// A missing snapshot clone must be refused BEFORE the live lease is touched.
func TestRestoreNativeCheckpointRefusesAMissingSnapshotBeforeMutation(t *testing.T) {
	runner := checkpointBaseRunner(t, func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		switch {
		case findArg(req.Args, "image", "inspect"):
			return core.LocalCommandResult{Stdout: checkpointTestNewImageID}, nil
		case findArg(req.Args, "volume", "ls", "--format"):
			return core.LocalCommandResult{Stdout: "some-other-volume\n"}, nil
		}
		return core.LocalCommandResult{}, nil
	})
	useCheckpointRuntime(t, runner)
	_, err := (Provider{}).RestoreNativeCheckpoint(t.Context(), core.NativeCheckpointRestoreRequest{
		Config: checkpointTestConfig(), Server: checkpointTestServer(), Target: checkpointTestTarget(),
		LeaseID: checkpointTestLeaseID, RepoName: "example",
		Record: core.NativeCheckpointRestoreRecord{
			Kind: core.CheckpointKindContainerdCommit, ImageID: checkpointTestNewImageID,
			Name: "crabbox-checkpoint-checkpoint-cp-0123456789ab", Metadata: checkpointTestMetadata(checkpointTestConfig()),
		},
	})
	var failure core.ExitError
	if !core.AsExitError(err, &failure) || failure.Code != 7 {
		t.Fatalf("err=%v, want exit 7 for a missing snapshot", err)
	}
	if hasRecordedCall(runner.calls, "rm", "-f", checkpointTestContainer) || hasRecordedCall(runner.calls, "volume", "rm") || hasRecordedCall(runner.calls, "run", "-d") {
		t.Fatal("a missing snapshot mutated the live lease")
	}
}

func TestParseCheckpointPorts(t *testing.T) {
	hostPort, extra := parseCheckpointPorts("2222/tcp -> 127.0.0.1:55283\n8080/tcp -> 127.0.0.1:55284\n9090/tcp -> 127.0.0.1:55285\n")
	if hostPort != "55283" {
		t.Fatalf("ssh port=%q", hostPort)
	}
	if len(extra) != 2 || extra[0] != "127.0.0.1:55284:8080" || extra[1] != "127.0.0.1:55285:9090" {
		t.Fatalf("extra ports=%v", extra)
	}
}

func TestHelperRuntimeUsesTheBaseEngineForKrun(t *testing.T) {
	if got := helperRuntimePath(core.ContainerdConfig{Runtime: "krun", RuntimePath: "/usr/bin/krun"}); got != "/usr/bin/crun" {
		t.Fatalf("krun helper runtime=%q", got)
	}
	if got := helperRuntimePath(core.ContainerdConfig{Runtime: "runc"}); got != "/usr/bin/runc" {
		t.Fatalf("runc helper runtime=%q", got)
	}
}

// Restore must prove the ambient configuration still reaches the recorded
// daemon BEFORE replaying the recorded settings, otherwise the comparison would
// compare the record with itself.
func TestRestoreNativeCheckpointRefusesScopeDrift(t *testing.T) {
	runner := checkpointBaseRunner(t, nil)
	useCheckpointRuntime(t, runner)
	ambient := checkpointTestConfig()
	ambient.Containerd.Runtime = "runc"
	metadata := checkpointTestMetadata(checkpointTestConfig())
	metadata[checkpointMetadataRuntime] = "crun"
	_, err := (Provider{}).RestoreNativeCheckpoint(t.Context(), core.NativeCheckpointRestoreRequest{
		Config: ambient, Server: checkpointTestServer(), Target: checkpointTestTarget(),
		LeaseID: checkpointTestLeaseID, RepoName: "example",
		Record: core.NativeCheckpointRestoreRecord{
			Kind: core.CheckpointKindContainerdCommit, ImageID: checkpointTestNewImageID,
			Name: "crabbox-checkpoint-checkpoint-cp-0123456789ab", Metadata: metadata,
		},
	})
	var failure core.ExitError
	if !core.AsExitError(err, &failure) || failure.Code != 7 {
		t.Fatalf("err=%v, want exit 7 scope drift", err)
	}
	if hasRecordedCall(runner.calls, "image", "inspect") || hasRecordedCall(runner.calls, "rm", "-f") || hasRecordedCall(runner.calls, "run", "-d") {
		t.Fatal("restore scope drift was detected after mutating the lease")
	}
}

// A restore is bound to the checkpoint's own lease: a different --id must be
// refused before any daemon call, or it would replace the recorded lease's
// container with another lease's identity.
func TestRestoreNativeCheckpointRefusesADifferentLease(t *testing.T) {
	runner := checkpointBaseRunner(t, nil)
	useCheckpointRuntime(t, runner)
	_, err := (Provider{}).RestoreNativeCheckpoint(t.Context(), core.NativeCheckpointRestoreRequest{
		Config: checkpointTestConfig(), Server: checkpointTestServer(), Target: checkpointTestTarget(),
		LeaseID: "cbx_ffffffffffff", RepoName: "example",
		Record: core.NativeCheckpointRestoreRecord{
			Kind: core.CheckpointKindContainerdCommit, ImageID: checkpointTestNewImageID,
			Name: "crabbox-checkpoint-checkpoint-cp-0123456789ab", Metadata: checkpointTestMetadata(checkpointTestConfig()),
		},
	})
	var failure core.ExitError
	if !core.AsExitError(err, &failure) || failure.Code != 7 || !strings.Contains(err.Error(), "taken from lease") {
		t.Fatalf("err=%v, want exit 7 for a different lease", err)
	}
	runner.mu.Lock()
	calls := len(runner.calls)
	runner.mu.Unlock()
	if calls != 0 {
		t.Fatalf("a mismatched restore touched the daemon (%d calls)", calls)
	}
}

// The fork path validates the recorded tag against the daemon before Acquire
// launches, and clones the captured state volume into the new lease's volume.
func TestValidateCheckpointForkScopeAndClone(t *testing.T) {
	runner := checkpointBaseRunner(t, func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		switch {
		case findArg(req.Args, "image", "inspect"):
			return core.LocalCommandResult{Stdout: checkpointTestNewImageID}, nil
		case findArg(req.Args, "run", "--rm") && findArg(req.Args, "-v", "crabbox-cp-cp-0123456789ab-state:/src:ro"):
			return core.LocalCommandResult{}, nil
		}
		return core.LocalCommandResult{}, nil
	})
	useCheckpointRuntime(t, runner)
	cfg := checkpointTestConfig()
	cfg.Containerd.CheckpointMetadata = cloneCheckpointMetadata(checkpointTestMetadata(cfg))
	cfg.Containerd.CheckpointMetadata[checkpointMetadataForkImageID] = checkpointTestNewImageID
	cfg.Containerd.CheckpointMetadata[checkpointMetadataForkImageName] = "crabbox-checkpoint-checkpoint-cp-0123456789ab"
	info := daemonInfo{ID: checkpointTestDaemonID}
	if err := validateCheckpointForkScope(t.Context(), checkpointBackend(cfg).runner(), cfg, info); err != nil {
		t.Fatalf("matching fork scope: %v", err)
	}
	replaced := cloneCheckpointMetadata(cfg.Containerd.CheckpointMetadata)
	replaced[checkpointMetadataForkImageID] = checkpointTestImageID
	replacedCfg := cfg
	replacedCfg.Containerd.CheckpointMetadata = replaced
	if err := validateCheckpointForkScope(t.Context(), checkpointBackend(replacedCfg).runner(), replacedCfg, info); err == nil {
		t.Fatal("a replaced fork tag validated")
	}
	b := checkpointBackend(cfg)
	if err := b.cloneCheckpointState(t.Context(), cfg, "crabbox-cbx_ffffffffffff-state"); err != nil {
		t.Fatalf("clone: %v", err)
	}
	if !hasRecordedCall(runner.calls, "run", "--rm", "--runtime", "/usr/bin/crun") ||
		!hasRecordedCall(runner.calls, "-v", "crabbox-cp-cp-0123456789ab-state:/src:ro") ||
		!hasRecordedCall(runner.calls, "-v", "crabbox-cbx_ffffffffffff-state:/dst") {
		t.Fatalf("fork clone argv missing: %+v", runner.calls)
	}
}

// The lease SSH user is non-root on a stock image, and /etc/machine-id is
// root-owned: a capture that writes it without escalation fails with
// "Permission denied" before the commit starts (measured against the live
// rootless daemon). Every machine-id step must escalate through the
// passwordless sudo the container bootstrap grants the lease user.
func TestMachineIDStepsEscalateToRootForANonRootLease(t *testing.T) {
	steps := map[string]string{
		"empty":   checkpointEmptyMachineIDScript(),
		"restore": checkpointRestoreMachineIDScript(),
		"remove":  checkpointRemoveMachineIDScript(),
	}
	for name, script := range steps {
		if !strings.Contains(script, "sudo -n") {
			t.Errorf("%s machine-id step has no root escalation: %q", name, script)
		}
		if !strings.Contains(script, "as_root") {
			t.Errorf("%s machine-id step does not route its write through as_root: %q", name, script)
		}
	}
	if !strings.Contains(steps["empty"], "as_root sh -c ': > /etc/machine-id'") {
		t.Errorf("empty step does not escalate the write: %q", steps["empty"])
	}
	if !strings.Contains(steps["restore"], "as_root sh -c 'cat > /etc/machine-id'") {
		t.Errorf("restore step does not escalate the write: %q", steps["restore"])
	}
}

// Remove-before-clone is the normal path: the destination volume does not
// exist yet. Docker and nerdctl spell that differently, and matching only
// Docker's wording made the first capture of every lease fail on a volume that
// never existed (measured: 'volume "crabbox-cp-chk-…-state": not found').
func TestMissingVolumeIsToleratedInEveryDaemonWording(t *testing.T) {
	tolerated := map[string]core.LocalCommandResult{
		"docker":  {ExitCode: 1, Stderr: "Error response from daemon: no such volume: crabbox-cp-x-state"},
		"nerdctl": {ExitCode: 1, Stderr: "time=\"t\" level=warning msg=\"volume \\\"crabbox-cp-x-state\\\": not found\"\ntime=\"t\" level=fatal msg=\"some volumes could not be removed\""},
	}
	for name, result := range tolerated {
		if !checkpointVolumeMissing(result, errors.New("exit status 1")) {
			t.Errorf("%s: a missing volume must be tolerated", name)
		}
	}
	real := core.LocalCommandResult{ExitCode: 1, Stderr: "volume crabbox-cp-x-state is in use"}
	if checkpointVolumeMissing(real, errors.New("exit status 1")) {
		t.Error("an in-use volume was classified as missing")
	}
}

// A restore receives the lease SSH target from the CLI, so it never passes
// through sshTargetForPort. Without applying the lease readiness contract, the
// restore re-creates the container and then waits for the prepared-image marker
// no stock image ships (measured: a restore that timed out with
// authentication=ok and the port published).
func TestRestoreTargetGetsTheLeaseReadinessContract(t *testing.T) {
	cfg := checkpointTestConfig()
	got := checkpointReadyTarget(cfg, core.SSHTarget{})
	if got.ReadyCheck != leaseReadyCheck(cfg.Containerd.WorkRoot) {
		t.Fatalf("restore target ReadyCheck=%q want %q", got.ReadyCheck, leaseReadyCheck(cfg.Containerd.WorkRoot))
	}
	explicit := core.SSHTarget{ReadyCheck: "test -f /custom/ready"}
	if kept := checkpointReadyTarget(cfg, explicit); kept.ReadyCheck != explicit.ReadyCheck {
		t.Fatalf("an explicit ReadyCheck was overwritten: %q", kept.ReadyCheck)
	}
}
