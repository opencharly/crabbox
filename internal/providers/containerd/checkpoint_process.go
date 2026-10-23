package containerd

import (
	"context"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
	"github.com/openclaw/crabbox/internal/providers/shared/containercheckpoint"
)

// Process checkpoints are containerd's CRIU primitive: the container's task
// (processes, memory and namespaces) is dumped to the nerdctl data root while
// the writable layer stays exactly where it is. The name is derived from the
// lease so the durable record doubles as the ownership proof a delete needs.
const (
	criuPreflightCommand   = "criu check --unprivileged --feature seccomp_suspend"
	criuSeccompSupported   = "seccomp_suspend is supported"
	criuNoSubreaperFlag    = "no-subreaper"
	processCheckpointLabel = "crabbox-"
)

// closeProcessCheckpointSSHSessions ends the multiplexed SSH masters Crabbox
// holds for the lease. It is a variable so the provider tests can prove the
// close happens before the capture without standing up an SSH master.
var closeProcessCheckpointSSHSessions = core.CloseLeaseSSHSessions

// processCheckpointPrefix is the ownership prefix for one lease's process
// checkpoints: a delete may only touch a name derived from the recorded lease.
func processCheckpointPrefix(leaseID string) string {
	return processCheckpointLabel + containercheckpoint.NormalizeRepositoryComponent(leaseID, 40) + "-"
}

// processCheckpointName renders crabbox-<lease>-<short>. The checkpoint id is
// the discriminator, so two captures of one lease never collide.
func processCheckpointName(leaseID, checkpointID string) string {
	return processCheckpointPrefix(leaseID) + containercheckpoint.NormalizeRepositoryComponent(checkpointID, 24)
}

// checkpointResourceKind selects the primitive from the request or, when the
// caller did not carry it, from the durable metadata.
func checkpointResourceKind(image core.NativeCheckpointImage, metadata map[string]string) string {
	if kind := strings.TrimSpace(image.Kind); kind != "" {
		return kind
	}
	return strings.TrimSpace(metadata[checkpointMetadataKind])
}

// createProcessCheckpoint captures a running container's process tree with
// containerd's CRIU checkpoint. It never pauses, stops, commits or removes the
// source: the writable layer is not captured and is not rolled back.
func createProcessCheckpoint(ctx context.Context, cfg core.Config, req core.NativeCheckpointCreateRequest) (core.NativeCheckpointCreateResult, error) {
	b := checkpointBackend(cfg)
	info, err := b.verifyDaemon(ctx)
	if err != nil {
		return core.NativeCheckpointCreateResult{}, err
	}
	if req.Capture != nil {
		// The source-retirement journal would delete the very container the
		// checkpoint is bound to, leaving a process checkpoint that can never be
		// restored. Refuse before any mutation.
		return core.NativeCheckpointCreateResult{}, core.Exit(2, "containerd-criu process checkpoints are bound to their source container and cannot retire it; capture --strategy image for a source-retirement checkpoint")
	}
	container := checkpointContainerName(req.Server)
	if container == "" {
		return core.NativeCheckpointCreateResult{}, core.Exit(2, "containerd-criu checkpoint requires a running lease container")
	}
	runner := b.runner()
	status, err := checkpointContainerStatus(ctx, runner, container)
	if err != nil {
		return core.NativeCheckpointCreateResult{}, err
	}
	if status == "stopped" {
		return core.NativeCheckpointCreateResult{}, core.Exit(7, "refusing to checkpoint container %s: it has a zombie STOPPED task (containerd kept the task after it exited), and CRIU can only dump a live task. The writable layer is preserved; remove the lease and start a fresh one, or use --strategy image", container)
	}
	if status != "running" {
		return core.NativeCheckpointCreateResult{}, core.Exit(2, "containerd-criu process checkpoints require a running source container; container %s is %s. Process checkpoints capture live tasks only; use --strategy image for a stopped source", container, status)
	}
	if err := requireProcessCheckpointRuntime(ctx, runner, cfg); err != nil {
		return core.NativeCheckpointCreateResult{}, err
	}
	// CRIU dumps established sockets, so no Crabbox SSH session may be open into
	// the lease when the task is captured. Every provider command above runs on
	// the daemon host and has already completed; close the multiplexed masters
	// the lease's earlier commands left behind.
	if err := closeProcessCheckpointSSHSessions(ctx, req.Target); err != nil {
		return core.NativeCheckpointCreateResult{}, core.Exit(7, "close the lease's SSH sessions before the process checkpoint: %v", err)
	}
	name := processCheckpointName(req.LeaseID, req.CheckpointID)
	metadata := checkpointScopeMetadata(cfg, req.Server, req.LeaseID, info.ID, container)
	metadata[checkpointMetadataKind] = core.CheckpointKindContainerdCRIU
	metadata[checkpointMetadataProcessCheckpoint] = name
	metadata[checkpointMetadataCold] = "false"
	result, err := runner.nerdctlOutput(ctx, "checkpoint", "create", "--leave-running", container, name)
	if err != nil {
		return core.NativeCheckpointCreateResult{}, core.Exit(7, "nerdctl checkpoint create --leave-running %s %s: %v: %s", container, name, err, containercheckpoint.TrimFailure(result.Stderr))
	}
	return core.NativeCheckpointCreateResult{
		CaptureEstablished: true,
		Image: core.NativeCheckpointImage{
			ID: name, Name: name, State: "available", Provider: providerName,
			Kind: core.CheckpointKindContainerdCRIU, Direct: true,
		},
		Metadata: metadata,
	}, nil
}

// requireProcessCheckpointRuntime refuses every configuration whose mechanism
// this seam does not implement, then proves the CRIU pre-flight and the
// runtime's restore capability. Both probes run on the containerd host (the
// daemon's namespace), never inside the lease.
func requireProcessCheckpointRuntime(ctx context.Context, runner commandRunner, cfg core.Config) error {
	c := cfg.Containerd
	switch {
	case c.CRIU == "off":
		return core.Exit(2, "containerd.criu=off disables process checkpoints; set containerd.criu: on or auto to enable --strategy process")
	case c.CRIU == "in-container":
		return core.Exit(2, "containerd.criu=in-container runs CRIU inside the lease and is not offered by --strategy process; this seam checkpoints with the daemon host's CRIU against a rootful runc task")
	case c.Runtime == "krun":
		return core.Exit(2, "process checkpoints require containerd.runtime: runc on the daemon host; runtime krun runs the workload in a microVM guest, whose in-guest CRIU dump is not offered by this seam")
	case c.Runtime != "crun" && c.Runtime != "runc":
		return core.Exit(2, "process checkpoints require containerd.runtime: runc, not %q (crun is refused by the restore probe below)", c.Runtime)
	case c.Rootless:
		return core.Exit(2, "a rootless containerd daemon cannot process-checkpoint a seccomp-filtered task: the init user namespace is the gate, netns switch-back fails, and map_files is not readable. Use a rootful runc daemon")
	}
	output, rc, runErr := checkpointCRIUProbe(ctx, runner)
	if !checkpointCRIUProbeOK(rc, runErr, output) {
		return core.Exit(2, "containerd-criu needs `%s` to exit 0 AND print `%s` on the containerd host (rc=%d); the bulk `--extra` form is deliberately not used because it returns non-zero on hosts that checkpoint fine. Output: %s", criuPreflightCommand, criuSeccompSupported, rc, containercheckpoint.FirstValue(containercheckpoint.TrimFailure(output), "-"))
	}
	return requireProcessRestoreProbe(ctx, runner, cfg)
}

// checkpointCRIUProbe runs the pre-flight exactly once and returns the combined
// streams with the command's own exit code. The caller owns the pass condition
// so the doctor and the checkpoint seam cannot drift.
func checkpointCRIUProbe(ctx context.Context, runner commandRunner) (string, int, error) {
	result, err := runner.runShell(ctx, criuPreflightCommand+" 2>&1", nil, nil)
	output := result.Stdout
	if stderr := result.Stderr; strings.TrimSpace(stderr) != "" {
		output = strings.TrimSpace(output + "\n" + stderr)
	}
	return output, result.ExitCode, err
}

// checkpointCRIUProbeOK is the whole pre-flight condition: the command's own
// rc is 0 AND the supported line appears on either stream. rc 0 alone is not
// enough, and the line alone with a non-zero rc is not enough.
func checkpointCRIUProbeOK(rc int, runErr error, output string) bool {
	return runErr == nil && rc == 0 && strings.Contains(output, criuSeccompSupported)
}

// requireProcessRestoreProbe proves the configured runtime can restore what
// containerd creates: containerd invokes `restore` with -no-subreaper, and a
// runtime that rejects the flag (crun exits 64) would leave a checkpoint that
// create happily produced but start --checkpoint can never consume.
func requireProcessRestoreProbe(ctx context.Context, runner commandRunner, cfg core.Config) error {
	runtimePath := runtimePathForConfig(cfg.Containerd)
	result, err := runner.runShell(ctx, core.ShellQuote(runtimePath)+" restore --help 2>&1", nil, nil)
	output := result.Stdout
	if stderr := result.Stderr; strings.TrimSpace(stderr) != "" {
		output = strings.TrimSpace(output + "\n" + stderr)
	}
	if err != nil || result.ExitCode != 0 || !strings.Contains(output, criuNoSubreaperFlag) {
		return core.Exit(2, "runtime %s cannot restore a containerd process checkpoint: containerd passes -no-subreaper to `restore`, and this runtime's `restore --help` does not accept it (crun rejects it with exit 64). Use containerd.runtime: runc", runtimePath)
	}
	return nil
}

// verifyProcessCheckpoint reports whether the recorded checkpoint still exists
// for its recorded container on the recorded daemon.
func verifyProcessCheckpoint(ctx context.Context, req core.NativeCheckpointResourceRequest) (core.NativeCheckpointVerifyResult, error) {
	cfg, err := checkpointLoadConfig(req.LoadConfig)
	if err != nil {
		return core.NativeCheckpointVerifyResult{}, err
	}
	b := checkpointBackend(cfg)
	info, err := b.verifyDaemon(ctx)
	if err != nil {
		return core.NativeCheckpointVerifyResult{}, err
	}
	if err := validateCheckpointMetadata(cfg, info, req.Metadata); err != nil {
		return core.NativeCheckpointVerifyResult{}, err
	}
	container, name, err := processCheckpointIdentity(req.Metadata, req.Image.Name)
	if err != nil {
		return core.NativeCheckpointVerifyResult{}, err
	}
	names, listErr := checkpointProcessNames(ctx, b.runner(), container)
	if listErr != nil {
		if containerNotFound(listErr.result) {
			return core.NativeCheckpointVerifyResult{ProviderState: "missing", NextAction: "delete_local"}, nil
		}
		return core.NativeCheckpointVerifyResult{}, listErr.err
	}
	for _, candidate := range names {
		if candidate == name {
			return core.NativeCheckpointVerifyResult{ProviderState: "available", NextAction: "restore_or_delete"}, nil
		}
	}
	return core.NativeCheckpointVerifyResult{ProviderState: "missing", NextAction: "delete_local"}, nil
}

// deleteProcessCheckpoint removes exactly one named checkpoint after proving
// the recorded scope still matches and the name belongs to the recorded lease.
func deleteProcessCheckpoint(ctx context.Context, req core.NativeCheckpointResourceRequest) error {
	cfg, err := checkpointLoadConfig(req.LoadConfig)
	if err != nil {
		return err
	}
	b := checkpointBackend(cfg)
	info, err := b.verifyDaemon(ctx)
	if err != nil {
		return err
	}
	if err := validateCheckpointMetadata(cfg, info, req.Metadata); err != nil {
		return err
	}
	container, name, err := processCheckpointIdentity(req.Metadata, req.Image.Name)
	if err != nil {
		return err
	}
	recordedLease := strings.TrimSpace(req.Metadata[checkpointMetadataLease])
	if recordedLease == "" || !strings.HasPrefix(name, processCheckpointPrefix(recordedLease)) {
		return core.Exit(7, "refusing to delete checkpoint %s: its name is not derived from recorded lease %q, so ownership is not proven", name, recordedLease)
	}
	names, listErr := checkpointProcessNames(ctx, b.runner(), container)
	if listErr != nil {
		if containerNotFound(listErr.result) {
			return core.Exit(7, "refusing to delete checkpoint %s: container %s is gone, and nerdctl cannot reach a checkpoint without its container. The checkpoint remains under the nerdctl data root; remove it on the containerd host or restore the container first", name, container)
		}
		return listErr.err
	}
	found := false
	for _, candidate := range names {
		if candidate == name {
			found = true
			break
		}
	}
	if !found {
		// Already gone: deleting is idempotent, and there is no image or volume
		// owned by a process checkpoint.
		return nil
	}
	result, err := b.runner().nerdctlOutput(ctx, "checkpoint", "rm", container, name)
	if err != nil {
		return core.Exit(7, "nerdctl checkpoint rm %s %s: %v: %s", container, name, err, containercheckpoint.TrimFailure(result.Stderr))
	}
	return nil
}

// restoreProcessCheckpoint restores the recorded task into its own running or
// stopped container with `start --checkpoint`. It never recreates the
// container, clones a volume or removes anything: the writable layer stays as
// it is, which is the difference from the image primitive.
func restoreProcessCheckpoint(ctx context.Context, req core.NativeCheckpointRestoreRequest) (core.NativeCheckpointRestoreResult, error) {
	metadata := req.Record.Metadata
	recordedLease := strings.TrimSpace(metadata[checkpointMetadataLease])
	if recordedLease == "" {
		return core.NativeCheckpointRestoreResult{}, core.Exit(7, "refusing to restore: the checkpoint is missing its recorded lease id")
	}
	if !strings.EqualFold(recordedLease, strings.TrimSpace(req.LeaseID)) {
		return core.NativeCheckpointRestoreResult{}, core.Exit(7, "refusing to restore a checkpoint taken from lease %s into lease %s; fork it to create a new lease instead", recordedLease, req.LeaseID)
	}
	// Prove the AMBIENT configuration still reaches the recorded daemon before
	// replaying the recorded settings.
	ambient := req.Config
	ambient.Provider = providerName
	if strings.TrimSpace(ambient.Containerd.Runtime) == "" || strings.TrimSpace(ambient.Containerd.Namespace) == "" || strings.TrimSpace(ambient.Containerd.Snapshotter) == "" {
		return core.NativeCheckpointRestoreResult{}, core.Exit(7, "containerd checkpoint restore needs the namespace, runtime and snapshotter that identify the ambient daemon")
	}
	b := checkpointBackend(ambient)
	info, err := b.verifyDaemon(ctx)
	if err != nil {
		return core.NativeCheckpointRestoreResult{}, err
	}
	if err := validateCheckpointMetadata(ambient, info, metadata); err != nil {
		return core.NativeCheckpointRestoreResult{}, err
	}
	cfg := ambient
	applyCheckpointLeaseConfig(&cfg, metadata)
	if err := validateCheckpointConfig(cfg); err != nil {
		return core.NativeCheckpointRestoreResult{}, err
	}
	b = checkpointBackend(cfg)
	runner := b.runner()
	container, name, err := processCheckpointIdentity(metadata, req.Record.Name)
	if err != nil {
		return core.NativeCheckpointRestoreResult{}, err
	}
	if resolved := strings.TrimSpace(req.Server.Name); resolved != "" && !strings.EqualFold(resolved, container) {
		return core.NativeCheckpointRestoreResult{}, core.Exit(7, "refusing to restore checkpoint %s taken on container %s into resolved container %s", name, container, resolved)
	}
	if !strings.HasPrefix(name, processCheckpointPrefix(recordedLease)) {
		return core.NativeCheckpointRestoreResult{}, core.Exit(7, "refusing to restore checkpoint %s: its name is not derived from recorded lease %s, so ownership is not proven", name, recordedLease)
	}
	if err := requireProcessRestoreProbe(ctx, runner, cfg); err != nil {
		return core.NativeCheckpointRestoreResult{}, err
	}
	names, listErr := checkpointProcessNames(ctx, runner, container)
	if listErr != nil {
		if containerNotFound(listErr.result) {
			return core.NativeCheckpointRestoreResult{}, core.Exit(7, "refusing to restore: container %s is gone; the process checkpoint %s cannot be restored without it and the live lease was not changed", container, name)
		}
		return core.NativeCheckpointRestoreResult{}, listErr.err
	}
	present := false
	for _, candidate := range names {
		if candidate == name {
			present = true
			break
		}
	}
	if !present {
		return core.NativeCheckpointRestoreResult{}, core.Exit(7, "refusing to restore: checkpoint %s is missing from container %s; the live lease was not changed", name, container)
	}
	status, err := checkpointContainerStatus(ctx, runner, container)
	if err != nil {
		return core.NativeCheckpointRestoreResult{}, err
	}
	switch status {
	case "running":
		// stop deletes the task but keeps the container and its writable layer.
		result, stopErr := runner.nerdctlOutput(ctx, "stop", "--time", "30", container)
		if stopErr != nil && !containerNotFound(result) {
			return core.NativeCheckpointRestoreResult{}, core.Exit(7, "stop container %s before restoring checkpoint %s: %v: %s", container, name, stopErr, containercheckpoint.TrimFailure(result.Stderr))
		}
	case "paused":
		if result, pauseErr := runner.nerdctlOutput(ctx, "unpause", container); pauseErr != nil {
			return core.NativeCheckpointRestoreResult{}, core.Exit(7, "unpause container %s before restoring checkpoint %s: %v: %s", container, name, pauseErr, containercheckpoint.TrimFailure(result.Stderr))
		}
		if result, stopErr := runner.nerdctlOutput(ctx, "stop", "--time", "30", container); stopErr != nil && !containerNotFound(result) {
			return core.NativeCheckpointRestoreResult{}, core.Exit(7, "stop container %s before restoring checkpoint %s: %v: %s", container, name, stopErr, containercheckpoint.TrimFailure(result.Stderr))
		}
	case "stopped":
		return core.NativeCheckpointRestoreResult{}, core.Exit(7, "refusing to restore checkpoint %s: container %s has a zombie STOPPED task that containerd has not deleted, so `start --checkpoint` cannot create the restored task. The writable layer is preserved, not rolled back; remove the lease and start a fresh one", name, container)
	case "created", "exited":
		// Nothing is running; start --checkpoint creates the task directly.
	default:
		return core.NativeCheckpointRestoreResult{}, core.Exit(7, "refusing to restore checkpoint %s: container %s is in state %s", name, container, status)
	}
	// The restored task resurrects the captured sshd with its established
	// sockets, so the client's multiplexed masters must be gone first.
	if err := closeProcessCheckpointSSHSessions(ctx, req.Target); err != nil {
		return core.NativeCheckpointRestoreResult{}, core.Exit(7, "close the lease's SSH sessions before the process restore: %v", err)
	}
	result, err := runner.nerdctlOutput(ctx, "start", "--checkpoint", name, container)
	if err != nil {
		return core.NativeCheckpointRestoreResult{}, core.Exit(7, "nerdctl start --checkpoint %s %s: %v: %s", name, container, err, containercheckpoint.TrimFailure(result.Stderr))
	}
	readyTarget := checkpointReadyTarget(cfg, req.Target)
	if err := waitForCheckpointSSH(ctx, &readyTarget, providerOperationRuntime(nil).Stderr, "containerd process restore ssh", core.BootstrapWaitTimeout(cfg)); err != nil {
		return core.NativeCheckpointRestoreResult{}, err
	}
	if err := b.runReadinessHook(ctx, req.LeaseID, req.Target); err != nil {
		return core.NativeCheckpointRestoreResult{}, err
	}
	return core.NativeCheckpointRestoreResult{
		Converged: true,
		Message:   "container=" + container + " checkpoint=" + name + " writable_layer=preserved readiness=ok",
	}, nil
}

// processCheckpointIdentity resolves the recorded container and checkpoint
// name, refusing a record that cannot name either.
func processCheckpointIdentity(metadata map[string]string, name string) (string, string, error) {
	container := strings.TrimSpace(metadata[checkpointMetadataContainer])
	if container == "" {
		return "", "", core.Exit(7, "containerd process checkpoint is missing its recorded container; refusing the operation")
	}
	name = containercheckpoint.FirstValue(name, metadata[checkpointMetadataProcessCheckpoint])
	if name == "" {
		return "", "", core.Exit(7, "containerd process checkpoint is missing its recorded checkpoint name; refusing the operation")
	}
	return container, name, nil
}

// checkpointListError keeps the raw result beside the error so a caller can
// distinguish "the container is gone" from a real listing failure.
type checkpointListError struct {
	result core.LocalCommandResult
	err    error
}

// checkpointProcessNames lists the checkpoint names containerd reports for one
// container. nerdctl has no machine-readable format here, so the table header
// is skipped and the first field of every other row is the name.
func checkpointProcessNames(ctx context.Context, runner commandRunner, container string) ([]string, *checkpointListError) {
	result, err := runner.nerdctlOutput(ctx, "checkpoint", "ls", container)
	if err != nil {
		return nil, &checkpointListError{result: result, err: err}
	}
	return parseCheckpointNames(result.Stdout), nil
}

func parseCheckpointNames(output string) []string {
	var names []string
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if strings.EqualFold(fields[0], "CHECKPOINT") || strings.EqualFold(fields[0], "NAME") {
			continue
		}
		names = append(names, fields[0])
	}
	return names
}

// checkpointListEmpty reports nerdctl's `checkpoint ls` quirk: when the
// checkpoint base directory has never been created (no checkpoints exist on
// the host/namespace), nerdctl returns
// `checkpoint  does not exist for container <id>` — an EMPTY checkpoint name —
// instead of an empty list. For release that means "nothing to remove", not a
// failure; every checkpoint-free lease would otherwise never release.
func checkpointListEmpty(result core.LocalCommandResult) bool {
	return strings.Contains(result.Stderr+"\n"+result.Stdout, "checkpoint  does not exist for container")
}

// removeOwnedProcessCheckpoints deletes this lease's process checkpoints
// BEFORE the container is removed. A checkpoint is scoped to its container
// under the nerdctl data root, so removing the container first would leave an
// unreachable checkpoint behind. Ownership is the name prefix derived from the
// lease; a checkpoint belonging to another lease is never touched.
func (b *backend) removeOwnedProcessCheckpoints(ctx context.Context, leaseID, container string) error {
	leaseID = strings.TrimSpace(leaseID)
	container = strings.TrimSpace(container)
	if leaseID == "" || container == "" {
		return nil
	}
	names, listErr := checkpointProcessNames(ctx, b.runner(), container)
	if listErr != nil {
		if containerNotFound(listErr.result) || checkpointListEmpty(listErr.result) {
			return nil
		}
		return shared.LocalCommandError("nerdctl checkpoint ls", listErr.result, listErr.err)
	}
	prefix := processCheckpointPrefix(leaseID)
	for _, name := range names {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		result, err := b.runner().nerdctlOutput(ctx, "checkpoint", "rm", container, name)
		if err != nil {
			return core.Exit(7, "nerdctl checkpoint rm %s %s before removing the container: %v: %s", container, name, err, containercheckpoint.TrimFailure(result.Stderr))
		}
	}
	return nil
}
