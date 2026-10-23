package containerd

import (
	"context"
	"errors"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	processTestCheckpointID = "chk_0123456789ab"
	processTestName         = "crabbox-cbx-0123456789ab-chk-0123456789ab"
	processTestRuntimePath  = "/usr/bin/runc"
	processTestRuncHelp     = "Usage:\n  runc restore [options] <container-id>\n\nOptions:\n  --no-subreaper\n"
)

// processTestConfig is a rootful runc daemon with CRIU enabled: the only
// configuration the process seam accepts.
func processTestConfig() core.Config {
	cfg := checkpointTestConfig()
	cfg.Containerd.Runtime = "runc"
	cfg.Containerd.RuntimePath = processTestRuntimePath
	cfg.Containerd.CRIU = "on"
	return cfg
}

// recordSessionClose installs a session-close hook that records itself in the
// same call log as the subprocesses, so ordering is provable.
func recordSessionClose(t *testing.T, runner *recordingRunner) {
	t.Helper()
	previous := closeProcessCheckpointSSHSessions
	closeProcessCheckpointSSHSessions = func(context.Context, core.SSHTarget) error {
		runner.mu.Lock()
		runner.calls = append(runner.calls, core.LocalCommandRequest{Name: "close-ssh-sessions"})
		runner.mu.Unlock()
		return nil
	}
	t.Cleanup(func() { closeProcessCheckpointSSHSessions = previous })
}

func processCreateRequest() core.NativeCheckpointCreateRequest {
	return core.NativeCheckpointCreateRequest{
		Config: processTestConfig(), Server: checkpointTestServer(), Target: checkpointTestTarget(),
		CheckpointID: processTestCheckpointID, LeaseID: checkpointTestLeaseID, Name: "checkpoint", RepoName: "example",
		Strategy: core.CheckpointStrategyProcess,
	}
}

func isCRIUProbe(req core.LocalCommandRequest) bool {
	return req.Name == "sh" && strings.Contains(strings.Join(req.Args, " "), criuPreflightCommand)
}

func isRestoreProbe(req core.LocalCommandRequest) bool {
	return req.Name == "sh" && strings.Contains(strings.Join(req.Args, " "), "restore --help")
}

func isProcessCheckpointCreate(req core.LocalCommandRequest) bool {
	return findArg(req.Args, "checkpoint", "create")
}

// processCreateRunner answers every read the process capture makes and returns
// the caller's probe results.
func processCreateRunner(t *testing.T, runner *recordingRunner, status string, criu, restore core.LocalCommandResult) {
	t.Helper()
	recordSessionClose(t, runner)
	runner.run = func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		switch {
		case findArg(req.Args, "info"):
			output, _ := jsonInfo(daemonInfo{ID: checkpointTestDaemonID})
			return core.LocalCommandResult{Stdout: output}, nil
		case findArg(req.Args, "namespace", "ls"):
			return core.LocalCommandResult{Stdout: "default\n"}, nil
		case isCRIUProbe(req):
			return criu, nil
		case isRestoreProbe(req):
			return restore, nil
		case findArg(req.Args, "inspect", checkpointTestContainer, "--format", "{{.State.Status}}"):
			return core.LocalCommandResult{Stdout: status + "\n"}, nil
		case isProcessCheckpointCreate(req):
			return core.LocalCommandResult{}, nil
		}
		return core.LocalCommandResult{}, nil
	}
}

func newProcessRunner() *recordingRunner {
	return &recordingRunner{}
}

// The provider admits process only when it was explicitly requested; the
// implicit default stays the containerd-commit image and any other explicit
// strategy is refused rather than reinterpreted.
func TestNativeCheckpointCapabilitySelectsTheProcessPrimitive(t *testing.T) {
	for _, tc := range []struct {
		name     string
		strategy string
		explicit bool
		wantKind string
		wantOK   bool
	}{
		{name: "implicit default", strategy: "", explicit: false, wantKind: core.CheckpointKindContainerdCommit, wantOK: true},
		{name: "explicit image", strategy: core.CheckpointStrategyImage, explicit: true, wantKind: core.CheckpointKindContainerdCommit, wantOK: true},
		{name: "explicit process", strategy: core.CheckpointStrategyProcess, explicit: true, wantKind: core.CheckpointKindContainerdCRIU, wantOK: true},
		{name: "explicit disk", strategy: core.CheckpointStrategyDiskSnapshot, explicit: true, wantOK: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capability, ok := (Provider{}).NativeCheckpointCapability(core.NativeCheckpointRequest{
				Config: processTestConfig(), Server: checkpointTestServer(), Target: checkpointTestTarget(),
				Strategy: tc.strategy, StrategyExplicit: tc.explicit,
			})
			if ok != tc.wantOK {
				t.Fatalf("ok=%v want=%v capability=%+v", ok, tc.wantOK, capability)
			}
			if ok && capability.Kind != tc.wantKind {
				t.Fatalf("kind=%q want=%q", capability.Kind, tc.wantKind)
			}
		})
	}
}

// The whole capture order: CRIU pre-flight, runtime restore probe, close the
// lease's SSH sessions, then checkpoint create --leave-running. Nothing may
// pause, stop, commit or remove the source.
func TestCreateProcessCheckpointSequence(t *testing.T) {
	runner := newProcessRunner()
	processCreateRunner(t, runner, "running",
		core.LocalCommandResult{Stdout: criuSeccompSupported + "\n"},
		core.LocalCommandResult{Stdout: processTestRuncHelp},
	)
	useCheckpointRuntime(t, runner)
	result, err := (Provider{}).CreateNativeCheckpoint(t.Context(), processCreateRequest())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if result.Image.Kind != core.CheckpointKindContainerdCRIU ||
		result.Image.ID != processTestName || result.Image.Name != processTestName ||
		!result.CaptureEstablished {
		t.Fatalf("image=%+v captureEstablished=%v", result.Image, result.CaptureEstablished)
	}
	if result.Metadata[checkpointMetadataProcessCheckpoint] != processTestName ||
		result.Metadata[checkpointMetadataKind] != core.CheckpointKindContainerdCRIU ||
		result.Metadata[checkpointMetadataLease] != checkpointTestLeaseID ||
		result.Metadata[checkpointMetadataContainer] != checkpointTestContainer {
		t.Fatalf("metadata=%v", result.Metadata)
	}

	preflight := callIndex(runner, isCRIUProbe)
	restoreProbe := callIndex(runner, isRestoreProbe)
	closed := callIndex(runner, func(req core.LocalCommandRequest) bool { return req.Name == "close-ssh-sessions" })
	create := callIndex(runner, isProcessCheckpointCreate)
	for name, index := range map[string]int{"pre-flight": preflight, "restore-probe": restoreProbe, "close-sessions": closed, "checkpoint-create": create} {
		if index < 0 {
			t.Fatalf("%s did not run", name)
		}
	}
	if !(preflight < restoreProbe && restoreProbe < closed && closed < create) {
		t.Fatalf("order pre-flight=%d restore-probe=%d close=%d create=%d", preflight, restoreProbe, closed, create)
	}
	createCall, _ := callWith(runner, isProcessCheckpointCreate)
	if !findArg(createCall.Args, "checkpoint", "create", "--leave-running", checkpointTestContainer, processTestName) {
		t.Fatalf("checkpoint create argv=%v", createCall.Args)
	}
	// The capture is a live-process dump: it never asks CRIU for the bulk
	// --extra form, never pauses/stops/commits/removes the source, never uses
	// exec or --pid host, and never opens a guest SSH session.
	for _, call := range runner.calls {
		joined := strings.Join(call.Args, " ")
		for _, forbidden := range []string{"--extra", "exec", "--pid"} {
			if strings.Contains(joined, forbidden) {
				t.Fatalf("process capture used a forbidden invocation %q: %s", forbidden, joined)
			}
		}
		for _, verb := range []string{"pause", "stop", "commit", "rm -f", "rmi"} {
			if findArg(call.Args, strings.Fields(verb)...) {
				t.Fatalf("process capture ran %q: %v", verb, call.Args)
			}
		}
		if call.Name == "ssh" {
			t.Fatalf("process capture opened a guest SSH session: %v", call.Args)
		}
	}
}

// rc 0 alone is not the pre-flight: the supported line must be present, on
// whichever stream CRIU writes it. A non-zero rc with the line is a failure
// too. Every refusal happens before checkpoint create.
func TestCreateProcessCheckpointPreflightRequiresExitZeroAndTheLine(t *testing.T) {
	for _, tc := range []struct {
		name    string
		result  core.LocalCommandResult
		wantErr string
		pass    bool
	}{
		{name: "rc0 line on stdout", result: core.LocalCommandResult{Stdout: criuSeccompSupported + "\n"}, pass: true},
		{name: "rc0 line on stderr", result: core.LocalCommandResult{Stderr: criuSeccompSupported + "\n"}, pass: true},
		{name: "rc0 no line", result: core.LocalCommandResult{Stdout: "criu: feature probe finished\n"}, wantErr: criuSeccompSupported},
		{name: "nonzero with line", result: core.LocalCommandResult{ExitCode: 1, Stdout: criuSeccompSupported + "\n"}, wantErr: "rc=1"},
		{name: "nonzero no line", result: core.LocalCommandResult{ExitCode: 64, Stderr: "unknown option"}, wantErr: "rc=64"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := newProcessRunner()
			processCreateRunner(t, runner, "running", tc.result, core.LocalCommandResult{Stdout: processTestRuncHelp})
			useCheckpointRuntime(t, runner)
			_, err := (Provider{}).CreateNativeCheckpoint(t.Context(), processCreateRequest())
			if tc.pass {
				if err != nil {
					t.Fatalf("create: %v", err)
				}
				if !hasRecordedCall(runner.calls, "checkpoint", "create") {
					t.Fatal("a passing pre-flight did not reach checkpoint create")
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err=%v, want %q", err, tc.wantErr)
			}
			if hasRecordedCall(runner.calls, "checkpoint", "create") {
				t.Fatal("a failed pre-flight still ran checkpoint create")
			}
		})
	}
}

// containerd passes -no-subreaper to the runtime's restore; crun rejects it
// (exit 64), so a checkpoint it could create could never be restored. The
// refusal must name the mechanism, and nothing may be captured.
func TestCreateProcessCheckpointRefusesARuntimeThatCannotRestore(t *testing.T) {
	for _, tc := range []struct {
		name    string
		result  core.LocalCommandResult
		wantErr []string
	}{
		{
			name:    "crun rejects no-subreaper",
			result:  core.LocalCommandResult{ExitCode: 64, Stderr: "unknown flag: no-subreaper"},
			wantErr: []string{"no-subreaper", "exit 64"},
		},
		{
			name:    "help without the flag",
			result:  core.LocalCommandResult{Stdout: "Usage: crun restore CONTAINER\n"},
			wantErr: []string{"no-subreaper", "runc"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := newProcessRunner()
			processCreateRunner(t, runner, "running",
				core.LocalCommandResult{Stdout: criuSeccompSupported + "\n"}, tc.result)
			useCheckpointRuntime(t, runner)
			_, err := (Provider{}).CreateNativeCheckpoint(t.Context(), processCreateRequest())
			if err == nil {
				t.Fatal("a runtime that cannot restore was admitted")
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("err=%v, want %q", err, want)
				}
			}
			if hasRecordedCall(runner.calls, "checkpoint", "create") {
				t.Fatal("checkpoint create ran despite the restore refusal")
			}
		})
	}
}

// A zombie STOPPED task cannot be dumped: containerd has not deleted the task,
// so `checkpoint create` has no live task to capture. Refuse and report it.
func TestCreateProcessCheckpointRefusesAZombieStoppedTask(t *testing.T) {
	runner := newProcessRunner()
	processCreateRunner(t, runner, "stopped",
		core.LocalCommandResult{Stdout: criuSeccompSupported + "\n"},
		core.LocalCommandResult{Stdout: processTestRuncHelp})
	useCheckpointRuntime(t, runner)
	_, err := (Provider{}).CreateNativeCheckpoint(t.Context(), processCreateRequest())
	if err == nil || !strings.Contains(err.Error(), "zombie STOPPED") {
		t.Fatalf("err=%v, want the zombie STOPPED refusal", err)
	}
	if hasRecordedCall(runner.calls, "checkpoint", "create") || hasRecordedCall(runner.calls, "stop") {
		t.Fatal("a zombie task was mutated before the refusal")
	}
}

func TestCreateProcessCheckpointRefusesUnsupportedMechanisms(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*core.Config)
		want   string
	}{
		{"criu off", func(c *core.Config) { c.Containerd.CRIU = "off" }, "criu=off"},
		{"in-container", func(c *core.Config) { c.Containerd.CRIU = "in-container" }, "in-container"},
		{"krun in-guest", func(c *core.Config) { c.Containerd.Runtime = "krun" }, "krun"},
		{"unknown runtime", func(c *core.Config) { c.Containerd.Runtime = "youki" }, "runtime: runc"},
		{"rootless", func(c *core.Config) { c.Containerd.Rootless = true }, "rootless"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := newProcessRunner()
			processCreateRunner(t, runner, "running",
				core.LocalCommandResult{Stdout: criuSeccompSupported + "\n"},
				core.LocalCommandResult{Stdout: processTestRuncHelp})
			useCheckpointRuntime(t, runner)
			req := processCreateRequest()
			tc.mutate(&req.Config)
			_, err := (Provider{}).CreateNativeCheckpoint(t.Context(), req)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want %q", err, tc.want)
			}
			if hasRecordedCall(runner.calls, "checkpoint", "create") {
				t.Fatal("an unsupported mechanism still captured a checkpoint")
			}
		})
	}
}

// A process checkpoint is bound to its container, so it cannot be part of a
// source-retirement capture (which removes that container).
func TestCreateProcessCheckpointRefusesSourceRetirement(t *testing.T) {
	runner := newProcessRunner()
	processCreateRunner(t, runner, "running",
		core.LocalCommandResult{Stdout: criuSeccompSupported + "\n"},
		core.LocalCommandResult{Stdout: processTestRuncHelp})
	useCheckpointRuntime(t, runner)
	req := processCreateRequest()
	req.Capture = &core.NativeCheckpointCapture{SourceDisposition: "retire", Phase: "prepared"}
	_, err := (Provider{}).CreateNativeCheckpoint(t.Context(), req)
	if err == nil || !strings.Contains(err.Error(), "bound to their source container") {
		t.Fatalf("err=%v, want the source-retirement refusal", err)
	}
	if hasRecordedCall(runner.calls, "checkpoint", "create") {
		t.Fatal("source retirement still captured a process checkpoint")
	}
}

// Fork is refused with the mechanism: a CRIU dump is bound to its task and
// network namespace and restores only into the same container.
func TestForkRefusesProcessCheckpointsWithTheMechanism(t *testing.T) {
	cfg := processTestConfig()
	err := (Provider{}).ApplyNativeCheckpointForkConfig(core.NativeCheckpointForkRequest{
		Config: &cfg,
		Record: core.NativeCheckpointForkRecord{Kind: core.CheckpointKindContainerdCRIU, Name: processTestName},
	})
	if err == nil {
		t.Fatal("a process checkpoint was accepted for fork")
	}
	for _, want := range []string{"task", "network namespace", "start --checkpoint", "writable layer"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err=%v, want %q", err, want)
		}
	}
	if cfg.Containerd.Image != processTestConfig().Containerd.Image {
		t.Fatalf("a refused fork mutated the image: %q", cfg.Containerd.Image)
	}
}

func processRestoreRequest(metadata map[string]string) core.NativeCheckpointRestoreRequest {
	return core.NativeCheckpointRestoreRequest{
		Config: processTestConfig(), Server: checkpointTestServer(), Target: checkpointTestTarget(),
		LeaseID: checkpointTestLeaseID, RepoName: "example",
		Record: core.NativeCheckpointRestoreRecord{
			Kind: core.CheckpointKindContainerdCRIU, ImageID: processTestName, Name: processTestName,
			Metadata: metadata,
		},
	}
}

func processRestoreMetadata() map[string]string {
	cfg := processTestConfig()
	metadata := checkpointScopeMetadata(cfg, checkpointTestServer(), checkpointTestLeaseID, checkpointTestDaemonID, checkpointTestContainer)
	metadata[checkpointMetadataKind] = core.CheckpointKindContainerdCRIU
	metadata[checkpointMetadataProcessCheckpoint] = processTestName
	return metadata
}

func processRestoreRunner(t *testing.T, runner *recordingRunner, status, listed string, restore core.LocalCommandResult, restoreErr error) {
	t.Helper()
	recordSessionClose(t, runner)
	runner.run = func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		switch {
		case findArg(req.Args, "info"):
			output, _ := jsonInfo(daemonInfo{ID: checkpointTestDaemonID})
			return core.LocalCommandResult{Stdout: output}, nil
		case findArg(req.Args, "namespace", "ls"):
			return core.LocalCommandResult{Stdout: "default\n"}, nil
		case findArg(req.Args, "checkpoint", "ls", checkpointTestContainer):
			return core.LocalCommandResult{Stdout: "CHECKPOINT NAME\n" + listed + "\n"}, nil
		case findArg(req.Args, "inspect", checkpointTestContainer, "--format", "{{.State.Status}}"):
			return core.LocalCommandResult{Stdout: status + "\n"}, nil
		case isRestoreProbe(req):
			return restore, restoreErr
		case findArg(req.Args, "stop", "--time", "30", checkpointTestContainer):
			return core.LocalCommandResult{}, nil
		case findArg(req.Args, "start", "--checkpoint", processTestName, checkpointTestContainer):
			return core.LocalCommandResult{}, nil
		case req.Name == "ssh" && strings.Contains(strings.Join(req.Args, " "), "crabbox/ready"):
			return core.LocalCommandResult{ExitCode: 0}, nil
		}
		return core.LocalCommandResult{}, nil
	}
}

// Restore keeps the container, stops it without removing it, and starts the
// recorded checkpoint on it: the writable layer is preserved, never recreated.
func TestRestoreProcessCheckpointStartsTheCheckpointAndPreservesTheWritableLayer(t *testing.T) {
	runner := newProcessRunner()
	processRestoreRunner(t, runner, "running", processTestName, core.LocalCommandResult{Stdout: processTestRuncHelp}, nil)
	useCheckpointRuntime(t, runner)
	useCheckpointSSH(t)
	result, err := (Provider{}).RestoreNativeCheckpoint(t.Context(), processRestoreRequest(processRestoreMetadata()))
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !result.Converged || !strings.Contains(result.Message, "writable_layer=preserved") || !strings.Contains(result.Message, "readiness=ok") {
		t.Fatalf("result=%+v", result)
	}
	listed := callIndex(runner, func(req core.LocalCommandRequest) bool {
		return findArg(req.Args, "checkpoint", "ls", checkpointTestContainer)
	})
	stop := callIndex(runner, func(req core.LocalCommandRequest) bool {
		return findArg(req.Args, "stop", "--time", "30", checkpointTestContainer)
	})
	closed := callIndex(runner, func(req core.LocalCommandRequest) bool { return req.Name == "close-ssh-sessions" })
	start := callIndex(runner, func(req core.LocalCommandRequest) bool {
		return findArg(req.Args, "start", "--checkpoint", processTestName, checkpointTestContainer)
	})
	if listed < 0 || stop < 0 || closed < 0 || start < 0 || !(listed < stop && stop < closed && closed < start) {
		t.Fatalf("restore order list=%d stop=%d close=%d start=%d", listed, stop, closed, start)
	}
	// The writable layer is never rolled back: no removal, recreation, volume
	// work, image work or copy helper is part of a process restore.
	for _, call := range runner.calls {
		for _, forbidden := range [][]string{{"rm", "-f"}, {"volume"}, {"commit"}, {"run"}, {"rmi"}, {"exec"}, {"--pid"}} {
			if findArg(call.Args, forbidden...) {
				t.Fatalf("process restore ran %v: %v", forbidden, call.Args)
			}
		}
	}
}

// A zombie STOPPED task is refused and reported before any mutation.
func TestRestoreProcessCheckpointRefusesAZombieStoppedTask(t *testing.T) {
	runner := newProcessRunner()
	processRestoreRunner(t, runner, "stopped", processTestName, core.LocalCommandResult{Stdout: processTestRuncHelp}, nil)
	useCheckpointRuntime(t, runner)
	useCheckpointSSH(t)
	_, err := (Provider{}).RestoreNativeCheckpoint(t.Context(), processRestoreRequest(processRestoreMetadata()))
	if err == nil || !strings.Contains(err.Error(), "zombie STOPPED") || !strings.Contains(err.Error(), "writable layer is preserved") {
		t.Fatalf("err=%v, want the zombie STOPPED refusal", err)
	}
	if hasRecordedCall(runner.calls, "stop") || hasRecordedCall(runner.calls, "start") {
		t.Fatal("a zombie task was mutated before the refusal")
	}
}

// Restore refuses a crun runtime for the same mechanism as create.
func TestRestoreProcessCheckpointRefusesARuntimeThatCannotRestore(t *testing.T) {
	runner := newProcessRunner()
	processRestoreRunner(t, runner, "running", processTestName,
		core.LocalCommandResult{ExitCode: 64, Stderr: "unknown flag: no-subreaper"}, errors.New("exit status 64"))
	useCheckpointRuntime(t, runner)
	useCheckpointSSH(t)
	req := processRestoreRequest(processRestoreMetadata())
	req.Config.Containerd.Runtime = "crun"
	req.Config.Containerd.RuntimePath = "/usr/bin/crun"
	req.Record.Metadata[checkpointMetadataRuntime] = "crun"
	req.Record.Metadata[checkpointMetadataRuntimePath] = "/usr/bin/crun"
	_, err := (Provider{}).RestoreNativeCheckpoint(t.Context(), req)
	if err == nil || !strings.Contains(err.Error(), "no-subreaper") {
		t.Fatalf("err=%v, want the restore-probe refusal", err)
	}
	if hasRecordedCall(runner.calls, "stop") || hasRecordedCall(runner.calls, "start") {
		t.Fatal("a runtime that cannot restore still mutated the container")
	}
}

// Ownership is proven before any delete: a name not derived from the recorded
// lease is refused, and a matching name is removed through checkpoint rm.
func TestDeleteProcessCheckpointProvesOwnership(t *testing.T) {
	cfg := processTestConfig()
	metadata := processRestoreMetadata()
	load := func() (core.Config, error) { return cfg, nil }

	foreign := processRestoreRequest(metadata)
	foreign.Record.Name = "crabbox-cbx-ffffffffffff-chk-ffff"
	foreign.Record.ImageID = foreign.Record.Name
	runner := checkpointBaseRunner(t, func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		if findArg(req.Args, "checkpoint", "ls", checkpointTestContainer) {
			return core.LocalCommandResult{Stdout: "CHECKPOINT NAME\n" + foreign.Record.Name + "\n"}, nil
		}
		return core.LocalCommandResult{}, nil
	})
	useCheckpointRuntime(t, runner)
	err := (Provider{}).DeleteNativeCheckpoint(t.Context(), core.NativeCheckpointResourceRequest{
		LoadConfig: load, Image: core.NativeCheckpointImage{Kind: core.CheckpointKindContainerdCRIU, Name: foreign.Record.Name},
		Metadata: metadata,
	})
	if err == nil || !strings.Contains(err.Error(), "ownership is not proven") {
		t.Fatalf("err=%v, want the ownership refusal", err)
	}
	if hasRecordedCall(runner.calls, "checkpoint", "rm") {
		t.Fatal("a foreign checkpoint was removed")
	}

	owned := checkpointBaseRunner(t, func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		if findArg(req.Args, "checkpoint", "ls", checkpointTestContainer) {
			return core.LocalCommandResult{Stdout: "CHECKPOINT NAME\n" + processTestName + "\n"}, nil
		}
		return core.LocalCommandResult{}, nil
	})
	useCheckpointRuntime(t, owned)
	if err := (Provider{}).DeleteNativeCheckpoint(t.Context(), core.NativeCheckpointResourceRequest{
		LoadConfig: load, Image: core.NativeCheckpointImage{Kind: core.CheckpointKindContainerdCRIU, Name: processTestName},
		Metadata: metadata,
	}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !hasRecordedCall(owned.calls, "checkpoint", "rm", checkpointTestContainer, processTestName) {
		t.Fatalf("checkpoint rm argv missing: %+v", owned.calls)
	}
}

// Verify reports a missing checkpoint and an available one without touching
// anything.
func TestVerifyProcessCheckpointReportsMissingAndAvailable(t *testing.T) {
	cfg := processTestConfig()
	load := func() (core.Config, error) { return cfg, nil }
	for _, tc := range []struct {
		name   string
		listed string
		want   string
	}{
		{name: "available", listed: processTestName, want: "available"},
		{name: "missing", listed: "crabbox-cbx-0123456789ab-other", want: "missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := checkpointBaseRunner(t, func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
				if findArg(req.Args, "checkpoint", "ls", checkpointTestContainer) {
					return core.LocalCommandResult{Stdout: "CHECKPOINT NAME\n" + tc.listed + "\n"}, nil
				}
				return core.LocalCommandResult{}, nil
			})
			useCheckpointRuntime(t, runner)
			result, err := (Provider{}).VerifyNativeCheckpoint(t.Context(), core.NativeCheckpointResourceRequest{
				LoadConfig: load, Image: core.NativeCheckpointImage{Kind: core.CheckpointKindContainerdCRIU, Name: processTestName},
				Metadata: processRestoreMetadata(),
			})
			if err != nil {
				t.Fatalf("verify: %v", err)
			}
			if result.ProviderState != tc.want {
				t.Fatalf("state=%q want=%q", result.ProviderState, tc.want)
			}
		})
	}
}

// Release removes only this lease's process checkpoints, and does it before
// stopping/removing the container they are scoped to.
func TestReleaseRemovesOwnedProcessCheckpointsBeforeTheContainer(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	runner := &recordingRunner{run: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		switch {
		case findArg(req.Args, "info"):
			output, _ := jsonInfo(daemonInfo{ID: checkpointTestDaemonID})
			return core.LocalCommandResult{Stdout: output}, nil
		case findArg(req.Args, "namespace", "ls"):
			return core.LocalCommandResult{Stdout: "default\n"}, nil
		case findArg(req.Args, "checkpoint", "ls", checkpointTestContainer):
			return core.LocalCommandResult{Stdout: "CHECKPOINT NAME\n" + processTestName + "\ncrabbox-cbx-ffffffffffff-chk-ffff\nforeign-lease-checkpoint\n"}, nil
		case findArg(req.Args, "checkpoint", "rm"):
			return core.LocalCommandResult{}, nil
		case findArg(req.Args, "stop"):
			return core.LocalCommandResult{}, nil
		case findArg(req.Args, "rm", "-f"):
			return core.LocalCommandResult{}, nil
		case findArg(req.Args, "ps", "-a", "--filter", "name=^"+checkpointTestContainer+"$"):
			return core.LocalCommandResult{}, nil
		}
		return core.LocalCommandResult{}, nil
	}}
	err := testBackend(runner).ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{
		LeaseID: checkpointTestLeaseID,
		Server:  core.Server{Name: checkpointTestContainer},
	}})
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	removeOwned := callIndex(runner, func(req core.LocalCommandRequest) bool {
		return findArg(req.Args, "checkpoint", "rm", checkpointTestContainer, processTestName)
	})
	stop := callIndex(runner, func(req core.LocalCommandRequest) bool { return findArg(req.Args, "stop") })
	removeContainer := callIndex(runner, func(req core.LocalCommandRequest) bool { return findArg(req.Args, "rm", "-f") })
	if removeOwned < 0 || stop < 0 || removeContainer < 0 || !(removeOwned < stop && stop < removeContainer) {
		t.Fatalf("release order checkpoint-rm=%d stop=%d rm=%d", removeOwned, stop, removeContainer)
	}
	for _, foreign := range []string{"crabbox-cbx-ffffffffffff-chk-ffff", "foreign-lease-checkpoint"} {
		if hasRecordedCall(runner.calls, "checkpoint", "rm", checkpointTestContainer, foreign) {
			t.Fatalf("release removed a checkpoint owned by another lease: %s", foreign)
		}
	}
}

// nerdctl`checkpoint ls` errors instead of returning an empty list when the
// checkpoint base directory has never existed; a checkpoint-free lease must
// still release.
func TestReleaseToleratesNerdctlEmptyCheckpointDirectory(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	runner := &recordingRunner{run: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		switch {
		case findArg(req.Args, "info"):
			output, _ := jsonInfo(daemonInfo{ID: checkpointTestDaemonID})
			return core.LocalCommandResult{Stdout: output}, nil
		case findArg(req.Args, "namespace", "ls"):
			return core.LocalCommandResult{Stdout: "default\n"}, nil
		case findArg(req.Args, "checkpoint", "ls", checkpointTestContainer):
			return core.LocalCommandResult{
				ExitCode: 1,
				Stderr:   `time="2026-10-23T20:56:10Z" level=fatal msg="checkpoint  does not exist for container ` + checkpointTestContainer + `"`,
			}, errors.New("exit status 1")
		case findArg(req.Args, "stop"):
			return core.LocalCommandResult{}, nil
		case findArg(req.Args, "rm", "-f"):
			return core.LocalCommandResult{}, nil
		case findArg(req.Args, "ps", "-a", "--filter", "name=^"+checkpointTestContainer+"$"):
			return core.LocalCommandResult{}, nil
		}
		return core.LocalCommandResult{}, nil
	}}
	if err := testBackend(runner).ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{
		LeaseID: checkpointTestLeaseID,
		Server:  core.Server{Name: checkpointTestContainer},
	}}); err != nil {
		t.Fatalf("a lease with no checkpoints must release: %v", err)
	}
	if hasRecordedCall(runner.calls, "checkpoint", "rm") {
		t.Fatal("no checkpoint rm may run when the checkpoint directory is absent")
	}
}

// Any other checkpoint-list failure is still a real release failure.
func TestReleaseStillFailsOnARealCheckpointListError(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	runner := &recordingRunner{run: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		switch {
		case findArg(req.Args, "info"):
			output, _ := jsonInfo(daemonInfo{ID: checkpointTestDaemonID})
			return core.LocalCommandResult{Stdout: output}, nil
		case findArg(req.Args, "namespace", "ls"):
			return core.LocalCommandResult{Stdout: "default\n"}, nil
		case findArg(req.Args, "checkpoint", "ls", checkpointTestContainer):
			return core.LocalCommandResult{ExitCode: 1, Stderr: "permission denied"}, errors.New("exit status 1")
		}
		return core.LocalCommandResult{}, nil
	}}
	err := testBackend(runner).ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{
		LeaseID: checkpointTestLeaseID,
		Server:  core.Server{Name: checkpointTestContainer},
	}})
	if err == nil {
		t.Fatal("a real checkpoint-list error must still fail release")
	}
}

// The doctor and the checkpoint seam share ONE pre-flight: rc 0 AND the
// supported line on either stream. A doctor that only looked at stdout or only
// at the exit code would report a host healthy that the capture refuses.
func TestDoctorCRIUProbeRequiresExitZeroAndTheLine(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result core.LocalCommandResult
		want   string
	}{
		{name: "line on stdout", result: core.LocalCommandResult{Stdout: criuSeccompSupported}, want: "ok"},
		{name: "line on stderr", result: core.LocalCommandResult{Stderr: criuSeccompSupported}, want: "ok"},
		{name: "rc0 no line", result: core.LocalCommandResult{Stdout: "probe finished"}, want: "failed"},
		{name: "nonzero with line", result: core.LocalCommandResult{ExitCode: 1, Stdout: criuSeccompSupported}, want: "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &recordingRunner{run: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
				joined := strings.Join(req.Args, " ")
				switch {
				case strings.Contains(joined, "--version"):
					return core.LocalCommandResult{Stdout: "nerdctl version 2.2.0"}, nil
				case strings.Contains(joined, "info"):
					return core.LocalCommandResult{Stdout: `{"ID":"daemon-id","SecurityOptions":["name=seccomp"]}`}, nil
				case strings.Contains(joined, "namespace ls"):
					return core.LocalCommandResult{Stdout: "default\n"}, nil
				case strings.Contains(joined, criuPreflightCommand):
					return tc.result, nil
				case strings.Contains(joined, "test -e"):
					return core.LocalCommandResult{ExitCode: 0}, nil
				}
				return core.LocalCommandResult{}, nil
			}}
			b := testBackend(runner)
			b.cfg.Containerd.CRIU = "on"
			result, err := b.Doctor(t.Context(), core.DoctorRequest{})
			if err != nil {
				t.Fatal(err)
			}
			for _, check := range result.Checks {
				if check.Check != "criu" {
					continue
				}
				if check.Status != tc.want {
					t.Fatalf("criu check=%+v want status %q", check, tc.want)
				}
				return
			}
			t.Fatalf("doctor reported no criu check: %+v", result.Checks)
		})
	}
}

func TestParseCheckpointNames(t *testing.T) {
	names := parseCheckpointNames("CHECKPOINT NAME\ncrabbox-cbx-0123456789ab-chk-0123456789ab\n\n")
	if len(names) != 1 || names[0] != processTestName {
		t.Fatalf("names=%v", names)
	}
}
