package containerd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
	"github.com/openclaw/crabbox/internal/providers/shared/containercheckpoint"
)

// Checkpoint metadata is the durable capture scope. Verify, delete, fork and
// restore all read it instead of trusting the ambient configuration: a
// checkpoint taken on one daemon must never be acted on through another.
const (
	checkpointMetadataRuntime        = "runtime"
	checkpointMetadataRuntimePath    = "runtime_path"
	checkpointMetadataRootless       = "rootless"
	checkpointMetadataNamespace      = "namespace"
	checkpointMetadataSnapshotter    = "snapshotter"
	checkpointMetadataAddress        = "address"
	checkpointMetadataHost           = "host"
	checkpointMetadataDaemon         = "daemon"
	checkpointMetadataContainer      = "container"
	checkpointMetadataLease          = "lease"
	checkpointMetadataStateVolume    = "state_volume"
	checkpointMetadataCheckpointVol  = "checkpoint_volume"
	checkpointMetadataImage          = "image"
	checkpointMetadataSSHUser        = "ssh_user"
	checkpointMetadataWorkRoot       = "work_root"
	checkpointMetadataInit           = "init"
	checkpointMetadataNested         = "nested"
	checkpointMetadataGPU            = "gpu"
	checkpointMetadataSeccomp        = "seccomp_profile"
	checkpointMetadataCPUs           = "cpus"
	checkpointMetadataMemory         = "memory"
	checkpointMetadataKrunGPUFlags   = "krun_gpu_flags"
	checkpointMetadataKrunHeadroom   = "krun_ram_headroom_mib"
	checkpointMetadataKrunNestedVirt = "krun_nested_virt"
	checkpointMetadataKrunNetwork    = "krun_network"
	checkpointMetadataKrunKernel     = "krun_kernel"
	checkpointMetadataDesktop        = "desktop"
	checkpointMetadataDesktopEnv     = "desktop_env"
	checkpointMetadataBrowser        = "browser"
	checkpointMetadataNetwork        = "network"
	checkpointMetadataInputsDir      = "inputs_dir"
	checkpointMetadataPublishedPort  = "published_port"
	checkpointMetadataExtraPorts     = "extra_ports"
	checkpointMetadataCold           = "cold"
	checkpointMetadataForkImageID    = "fork_image_id"
	checkpointMetadataForkImageName  = "fork_image_name"
)

// machineIDMissing marks a lease whose image had no /etc/machine-id at capture
// time, so the restore removes the file instead of recreating one.
const machineIDMissing = "\x00missing\x00"

// checkpointCleanupTimeout bounds the finally steps (unpause, machine-id
// restore, helper cleanup) so a broken guest cannot hang the capture.
const checkpointCleanupTimeout = 2 * time.Minute

func checkpointCleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), checkpointCleanupTimeout)
}

// providerOperationRuntime is the standard provider-operation executor. It is
// a variable so a test can drive the checkpoint sequence with a recording
// runner, exactly as the Acquire tests do.
var providerOperationRuntime = core.RuntimeForProviderOperation

// waitForCheckpointSSH is the readiness gate a restore waits on. It is a
// variable for the same reason: the provider test drives the sequence with a
// recording runner and cannot stand up a real sshd.
var waitForCheckpointSSH = core.WaitForSSHReady

// NativeCheckpointCapability declares the containerd-commit primitive. The
// commit is an image capture, so an explicit disk-snapshot request is refused
// rather than silently reinterpreted; an explicit image request is the same
// primitive the default selects.
func (Provider) NativeCheckpointCapability(req core.NativeCheckpointRequest) (core.NativeCheckpointCapability, bool) {
	if strings.TrimSpace(req.Server.CloudID) == "" {
		return core.NativeCheckpointCapability{}, false
	}
	if req.StrategyExplicit && strings.TrimSpace(req.Strategy) != core.CheckpointStrategyImage {
		return core.NativeCheckpointCapability{}, false
	}
	return core.NativeCheckpointCapability{Kind: core.CheckpointKindContainerdCommit, Direct: true}, true
}

// NativeCheckpointWorkdir resolves the lease workdir the archive fallback would
// capture, using the work root recorded on the lease.
func (Provider) NativeCheckpointWorkdir(req core.NativeCheckpointWorkdirRequest) string {
	if override := strings.TrimSpace(req.Override); override != "" {
		return override
	}
	cfg := req.Config
	if workRoot := strings.TrimSpace(req.Server.Labels["work_root"]); workRoot != "" {
		cfg.Containerd.WorkRoot = workRoot
	}
	return core.RemoteJoin(cfg, req.LeaseID, req.RepoName)
}

// CreateNativeCheckpoint captures the lease's root filesystem as a committed
// image and its state volume as a clone.
//
// The capture is consistent because the source is paused (or already stopped,
// a cold checkpoint) before the commit. /etc/machine-id is emptied first so a
// fork or restore boots with a fresh machine identity; a running source is
// restored in a finally block, whatever happened to the capture.
func (Provider) CreateNativeCheckpoint(ctx context.Context, req core.NativeCheckpointCreateRequest) (result core.NativeCheckpointCreateResult, err error) {
	cfg := req.Config
	cfg.Provider = providerName
	if err := validateCheckpointConfig(cfg); err != nil {
		return core.NativeCheckpointCreateResult{}, err
	}
	b := checkpointBackend(cfg)
	info, err := b.verifyDaemon(ctx)
	if err != nil {
		return core.NativeCheckpointCreateResult{}, err
	}
	container := checkpointContainerName(req.Server)
	if container == "" {
		return core.NativeCheckpointCreateResult{}, core.Exit(2, "containerd-commit checkpoint requires a running lease container")
	}
	metadata, err := checkpointMetadataForCreate(ctx, cfg, req, info, container)
	if err != nil {
		return core.NativeCheckpointCreateResult{}, err
	}
	runner := b.runner()
	status, err := checkpointContainerStatus(ctx, runner, container)
	if err != nil {
		return core.NativeCheckpointCreateResult{}, err
	}
	running := status == "running"
	metadata[checkpointMetadataCold] = strconv.FormatBool(!running)

	tag := checkpointRequestTag(req)
	var imageID string
	if running {
		if err := checkpointRequireGNUCopy(ctx, providerOperationRuntime(nil), req.Target); err != nil {
			return core.NativeCheckpointCreateResult{}, err
		}
		// Assign to the named err: the finally blocks below must join into the
		// RETURNED error, never into a shadowed copy.
		var originalMachineID string
		originalMachineID, err = checkpointEmptyMachineID(ctx, providerOperationRuntime(nil), req.Target)
		if err != nil {
			return core.NativeCheckpointCreateResult{}, err
		}
		defer func() {
			cleanupCtx, cancel := checkpointCleanupContext(ctx)
			defer cancel()
			if restoreErr := checkpointRestoreMachineID(cleanupCtx, providerOperationRuntime(nil), req.Target, originalMachineID); restoreErr != nil {
				err = errors.Join(err, restoreErr)
			}
		}()
		if err := checkpointPause(ctx, runner, container); err != nil {
			return core.NativeCheckpointCreateResult{}, err
		}
		defer func() {
			cleanupCtx, cancel := checkpointCleanupContext(ctx)
			defer cancel()
			if unpauseErr := checkpointUnpause(cleanupCtx, runner, container); unpauseErr != nil {
				err = errors.Join(err, unpauseErr)
			}
		}()
		imageID, err = checkpointCommit(ctx, runner, container, tag, containercheckpoint.BootableCommandChange())
	} else {
		imageID, err = checkpointCommitCold(ctx, b, cfg, container, tag)
	}
	if err != nil {
		return core.NativeCheckpointCreateResult{}, err
	}

	if volume := strings.TrimSpace(metadata[checkpointMetadataStateVolume]); volume != "" {
		if err := checkpointCloneVolume(ctx, runner, cfg, imageID, volume, metadata[checkpointMetadataCheckpointVol]); err != nil {
			return core.NativeCheckpointCreateResult{
				Image: core.NativeCheckpointImage{
					ID: imageID, Name: tag, State: "cleanup_failed", Provider: providerName,
					Kind: core.CheckpointKindContainerdCommit, Direct: true,
				},
				Metadata: metadata,
			}, err
		}
	}
	return core.NativeCheckpointCreateResult{
		CaptureEstablished: true,
		Image: core.NativeCheckpointImage{
			ID: imageID, Name: tag, State: "available", Provider: providerName,
			Kind: core.CheckpointKindContainerdCommit, Direct: true,
		},
		Metadata: metadata,
	}, nil
}

// VerifyNativeCheckpoint fails closed on scope drift and reports whether the
// recorded tag still resolves to the recorded image.
func (Provider) VerifyNativeCheckpoint(ctx context.Context, req core.NativeCheckpointResourceRequest) (core.NativeCheckpointVerifyResult, error) {
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
	currentID, missing, err := checkpointInspectImage(ctx, b.runner(), req.Image.Name)
	if err != nil {
		return core.NativeCheckpointVerifyResult{}, err
	}
	if missing {
		return core.NativeCheckpointVerifyResult{ProviderState: "missing", NextAction: "delete_local"}, nil
	}
	if !strings.EqualFold(currentID, req.Image.ID) {
		return core.NativeCheckpointVerifyResult{
			ProviderState: "conflict",
			NextAction:    "restore_tag_or_delete_local",
			Error:         fmt.Sprintf("checkpoint tag %s points to %s, recorded image is %s", req.Image.Name, currentID, req.Image.ID),
		}, nil
	}
	return core.NativeCheckpointVerifyResult{ProviderState: "available", NextAction: "fork_or_restore_or_delete"}, nil
}

// DeleteNativeCheckpoint removes the recorded tag only after proving it still
// names the recorded image, then removes the checkpoint's state-volume clone.
func (Provider) DeleteNativeCheckpoint(ctx context.Context, req core.NativeCheckpointResourceRequest) error {
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
	runner := b.runner()
	currentID, missing, err := checkpointInspectImage(ctx, runner, req.Image.Name)
	if err != nil {
		return err
	}
	if !missing {
		if !strings.EqualFold(currentID, req.Image.ID) {
			return core.Exit(7, "refusing to delete checkpoint tag %s: recorded image %s but tag now points to %s; restore the tag or use --local-only", req.Image.Name, req.Image.ID, currentID)
		}
		result, err := runner.nerdctlOutput(ctx, "rmi", "-f", req.Image.Name)
		if err != nil {
			// nerdctl reports a dependent container as a rmi failure; surface it
			// rather than reporting a clean delete.
			return core.Exit(7, "nerdctl rmi %s: %v: %s", req.Image.Name, err, containercheckpoint.TrimFailure(result.Stderr))
		}
	}
	if volume := strings.TrimSpace(req.Metadata[checkpointMetadataCheckpointVol]); volume != "" {
		if err := checkpointRemoveVolume(ctx, runner, volume); err != nil {
			return err
		}
	}
	return nil
}

// ApplyNativeCheckpointForkConfig starts a new lease from the recorded image
// and replays the recorded lease settings. Acquire clones the state volume.
func (Provider) ApplyNativeCheckpointForkConfig(req core.NativeCheckpointForkRequest) error {
	if req.Record.Kind != core.CheckpointKindContainerdCommit {
		return core.Exit(2, "provider=%s does not support checkpoint kind=%s", providerName, req.Record.Kind)
	}
	imageName := containercheckpoint.FirstValue(req.Record.Name, req.Record.Resource)
	if imageName == "" {
		return core.Exit(2, "containerd checkpoint fork requires a committed image tag")
	}
	if strings.TrimSpace(req.Record.ImageID) == "" {
		return core.Exit(2, "containerd checkpoint fork requires a recorded image id")
	}
	metadata := cloneCheckpointMetadata(req.Record.Metadata)
	applyCheckpointLeaseConfig(req.Config, metadata)
	metadata[checkpointMetadataForkImageID] = strings.TrimSpace(req.Record.ImageID)
	metadata[checkpointMetadataForkImageName] = imageName
	req.Config.Containerd.Image = imageName
	req.Config.Containerd.CheckpointMetadata = metadata
	return nil
}

// RestoreNativeCheckpoint replaces the state of the checkpoint's own lease:
// the container is recreated from the committed image and the state volume is
// re-cloned. The lease id, claim, SSH key and published ports are preserved.
func (Provider) RestoreNativeCheckpoint(ctx context.Context, req core.NativeCheckpointRestoreRequest) (core.NativeCheckpointRestoreResult, error) {
	// Prove the AMBIENT configuration still reaches the recorded daemon before
	// replaying the recorded lease settings; replaying first would make the
	// scope comparison compare the record with itself.
	ambient := req.Config
	ambient.Provider = providerName
	if strings.TrimSpace(ambient.Containerd.Runtime) == "" || strings.TrimSpace(ambient.Containerd.Namespace) == "" || strings.TrimSpace(ambient.Containerd.Snapshotter) == "" {
		return core.NativeCheckpointRestoreResult{}, core.Exit(7, "containerd checkpoint restore needs the namespace, runtime and snapshotter that identify the ambient daemon")
	}
	metadata := req.Record.Metadata
	b := checkpointBackend(ambient)
	info, err := b.verifyDaemon(ctx)
	if err != nil {
		return core.NativeCheckpointRestoreResult{}, err
	}
	if err := validateCheckpointMetadata(ambient, info, metadata); err != nil {
		return core.NativeCheckpointRestoreResult{}, err
	}
	// The recreate itself replays the recorded lease settings.
	cfg := ambient
	applyCheckpointLeaseConfig(&cfg, metadata)
	cfg.Containerd.Image = containercheckpoint.FirstValue(req.Record.Name, req.Record.Resource)
	if err := validateCheckpointConfig(cfg); err != nil {
		return core.NativeCheckpointRestoreResult{}, err
	}
	b = checkpointBackend(cfg)
	runner := b.runner()
	currentID, missing, err := checkpointInspectImage(ctx, runner, cfg.Containerd.Image)
	if err != nil {
		return core.NativeCheckpointRestoreResult{}, err
	}
	if missing {
		return core.NativeCheckpointRestoreResult{}, core.Exit(7, "refusing to restore: checkpoint image %s is missing", cfg.Containerd.Image)
	}
	if !strings.EqualFold(currentID, req.Record.ImageID) {
		return core.NativeCheckpointRestoreResult{}, core.Exit(7, "refusing to restore checkpoint tag %s: recorded image %s but tag now points to %s", cfg.Containerd.Image, req.Record.ImageID, currentID)
	}
	container := containercheckpoint.FirstValue(metadata[checkpointMetadataContainer], req.Server.Name, req.Server.Labels["container"])
	if container == "" {
		return core.NativeCheckpointRestoreResult{}, core.Exit(2, "containerd checkpoint restore requires the recorded lease container")
	}
	stateVolume := containercheckpoint.FirstValue(metadata[checkpointMetadataStateVolume], req.Server.Labels["state_volume"])
	checkpointVolume := strings.TrimSpace(metadata[checkpointMetadataCheckpointVol])
	if stateVolume != "" && checkpointVolume != "" {
		present, err := checkpointVolumeExists(ctx, runner, checkpointVolume)
		if err != nil {
			return core.NativeCheckpointRestoreResult{}, err
		}
		if !present {
			// Refuse before touching the live lease: a missing clone would turn a
			// converged restore into silent state loss.
			return core.NativeCheckpointRestoreResult{}, core.Exit(7, "refusing to restore: checkpoint state volume %s is missing; the live lease was not changed", checkpointVolume)
		}
	}
	inputsDir := containercheckpoint.FirstValue(req.Server.Labels["inputs_dir"], metadata[checkpointMetadataInputsDir])
	if inputsDir == "" {
		return core.NativeCheckpointRestoreResult{}, core.Exit(7, "refusing to restore: the lease has no recorded inputs directory")
	}
	if result, err := runner.runShell(ctx, "test -e "+core.ShellQuote(inputsDir), nil, nil); err != nil || result.ExitCode != 0 {
		return core.NativeCheckpointRestoreResult{}, core.Exit(7, "refusing to restore: the lease inputs directory %s is missing; the live lease was not changed", inputsDir)
	}
	hostPort, extraPorts, err := checkpointRecreatePorts(ctx, runner, container, metadata)
	if err != nil {
		return core.NativeCheckpointRestoreResult{}, err
	}
	if err := checkpointRemoveContainer(ctx, runner, container); err != nil {
		return core.NativeCheckpointRestoreResult{}, err
	}
	if stateVolume != "" {
		if err := checkpointRemoveVolume(ctx, runner, stateVolume); err != nil {
			return core.NativeCheckpointRestoreResult{}, err
		}
	}
	if stateVolume != "" && checkpointVolume != "" {
		if err := checkpointCloneVolume(ctx, runner, cfg, cfg.Containerd.Image, checkpointVolume, stateVolume); err != nil {
			return core.NativeCheckpointRestoreResult{}, err
		}
	}
	slug := req.Server.Labels["slug"]
	if slug == "" {
		slug = core.ServerSlug(req.Server)
	}
	labels := shared.CloneLabels(req.Server.Labels)
	args, err := b.buildRunArgsWithLabels(ctx, req.LeaseID, slug, container, inputsDir, stateVolume, hostPort, extraPorts, info, labels)
	if err != nil {
		return core.NativeCheckpointRestoreResult{}, err
	}
	runResult, err := runner.run(ctx, args, nil, nil)
	if err != nil || strings.TrimSpace(runResult.Stdout) == "" {
		return core.NativeCheckpointRestoreResult{}, core.Exit(7, "recreate the restored container %s: %v: %s", container, err, containercheckpoint.TrimFailure(runResult.Stderr))
	}
	if !runner.remote() {
		if err := b.waitForPort(ctx, hostPort); err != nil {
			return core.NativeCheckpointRestoreResult{}, err
		}
	}
	if err := waitForCheckpointSSH(ctx, &req.Target, providerOperationRuntime(nil).Stderr, "containerd restore ssh", core.BootstrapWaitTimeout(cfg)); err != nil {
		return core.NativeCheckpointRestoreResult{}, err
	}
	if err := b.runReadinessHook(ctx, req.LeaseID, req.Target); err != nil {
		return core.NativeCheckpointRestoreResult{}, err
	}
	return core.NativeCheckpointRestoreResult{
		Converged: true,
		Message:   fmt.Sprintf("container=%s image=%s state_volume=%s readiness=ok", container, cfg.Containerd.Image, stateVolume),
	}, nil
}

// ---- helpers ----

func checkpointBackend(cfg core.Config) *backend {
	if strings.TrimSpace(cfg.Provider) == "" {
		cfg.Provider = providerName
	}
	return newBackend(Provider{}.Spec(), cfg, providerOperationRuntime(nil)).(*backend)
}

func checkpointLoadConfig(load func() (core.Config, error)) (core.Config, error) {
	if load == nil {
		return core.Config{}, core.Exit(7, "containerd checkpoint operation has no configuration loader")
	}
	cfg, err := load()
	if err != nil {
		return core.Config{}, err
	}
	cfg.Provider = providerName
	return cfg, nil
}

func validateCheckpointConfig(cfg core.Config) error {
	c := cfg.Containerd
	if strings.TrimSpace(c.Namespace) == "" || strings.TrimSpace(c.Runtime) == "" || strings.TrimSpace(c.Snapshotter) == "" {
		return core.Exit(2, "containerd-commit checkpoints require the namespace, runtime and snapshotter that identify the daemon")
	}
	if strings.TrimSpace(c.Image) == "" {
		return core.Exit(2, "containerd-commit checkpoints require the lease image")
	}
	return nil
}

// checkpointMetadataForCreate records the exact capture scope and the lease
// settings a fork or restore must reproduce.
func checkpointMetadataForCreate(ctx context.Context, cfg core.Config, req core.NativeCheckpointCreateRequest, info daemonInfo, container string) (map[string]string, error) {
	labels := req.Server.Labels
	c := cfg.Containerd
	// The exact published mappings are replayed by a restore: re-allocating
	// would change the claim's SSH port and drop the lease's exposures.
	hostPort, extraPorts := "", []string{}
	portRunner := commandRunner{cfg: cfg, rt: providerOperationRuntime(nil)}
	if result, err := portRunner.nerdctlOutput(ctx, "port", container); err == nil {
		hostPort, extraPorts = parseCheckpointPorts(result.Stdout)
	}
	if hostPort == "" {
		hostPort = strings.TrimSpace(labels["published_port"])
	}
	encodedExtra := "[]"
	if encoded, err := json.Marshal(extraPorts); err == nil {
		encodedExtra = string(encoded)
	}
	metadata := map[string]string{
		checkpointMetadataRuntime:        c.Runtime,
		checkpointMetadataRuntimePath:    runtimePathForConfig(c),
		checkpointMetadataRootless:       strconv.FormatBool(c.Rootless),
		checkpointMetadataNamespace:      c.Namespace,
		checkpointMetadataSnapshotter:    c.Snapshotter,
		checkpointMetadataAddress:        c.Address,
		checkpointMetadataHost:           hostScopeForConfig(cfg),
		checkpointMetadataDaemon:         info.ID,
		checkpointMetadataContainer:      container,
		checkpointMetadataLease:          req.LeaseID,
		checkpointMetadataStateVolume:    strings.TrimSpace(labels["state_volume"]),
		checkpointMetadataCheckpointVol:  containercheckpoint.VolumeName(req.CheckpointID),
		checkpointMetadataImage:          c.Image,
		checkpointMetadataSSHUser:        c.User,
		checkpointMetadataWorkRoot:       c.WorkRoot,
		checkpointMetadataInit:           c.InitMode,
		checkpointMetadataNested:         c.Nested,
		checkpointMetadataGPU:            c.GPU,
		checkpointMetadataSeccomp:        c.SeccompProfile,
		checkpointMetadataCPUs:           strconv.Itoa(c.CPUs),
		checkpointMetadataMemory:         c.Memory,
		checkpointMetadataKrunGPUFlags:   strconv.Itoa(c.KrunGPUFlags),
		checkpointMetadataKrunHeadroom:   strconv.Itoa(c.KrunRAMHeadroomMiB),
		checkpointMetadataKrunNestedVirt: strconv.FormatBool(c.KrunNestedVirt),
		checkpointMetadataKrunNetwork:    c.KrunNetwork,
		checkpointMetadataKrunKernel:     c.KrunKernel,
		checkpointMetadataDesktop:        strconv.FormatBool(firstCheckpointLabelBool(labels["desktop"], cfg.Desktop)),
		checkpointMetadataDesktopEnv:     core.NormalizedDesktopEnv(containercheckpoint.FirstValue(labels["desktop_env"], cfg.DesktopEnv)),
		checkpointMetadataBrowser:        strconv.FormatBool(firstCheckpointLabelBool(labels["browser"], cfg.Browser)),
		checkpointMetadataNetwork:        c.Network,
		checkpointMetadataInputsDir:      strings.TrimSpace(labels["inputs_dir"]),
		checkpointMetadataPublishedPort:  hostPort,
		checkpointMetadataExtraPorts:     encodedExtra,
	}
	return metadata, nil
}

// validateCheckpointForkScope re-proves, at Acquire time, that the fork's
// recorded image still exists on the recorded daemon and still names the
// recorded digest.
func validateCheckpointForkScope(ctx context.Context, runner commandRunner, cfg core.Config, info daemonInfo) error {
	metadata := cfg.Containerd.CheckpointMetadata
	if len(metadata) == 0 {
		return nil
	}
	if err := validateCheckpointMetadata(cfg, info, metadata); err != nil {
		return err
	}
	tag := containercheckpoint.FirstValue(metadata[checkpointMetadataForkImageName], cfg.Containerd.Image)
	imageID := strings.TrimSpace(metadata[checkpointMetadataForkImageID])
	if tag == "" || imageID == "" {
		return core.Exit(7, "containerd checkpoint fork is missing its recorded image identity")
	}
	current, missing, err := checkpointInspectImage(ctx, runner, tag)
	if err != nil {
		return err
	}
	if missing {
		return core.Exit(7, "containerd checkpoint image %s is missing", tag)
	}
	if !strings.EqualFold(current, imageID) {
		return core.Exit(7, "refusing to fork checkpoint tag %s: recorded image %s but tag now points to %s", tag, imageID, current)
	}
	return nil
}

// cloneCheckpointState clones a checkpoint's captured state volume into the
// new lease's volume before the container starts.
func (b *backend) cloneCheckpointState(ctx context.Context, cfg core.Config, stateVolume string) error {
	source := strings.TrimSpace(cfg.Containerd.CheckpointMetadata[checkpointMetadataCheckpointVol])
	if source == "" || stateVolume == "" {
		return nil
	}
	return checkpointCloneVolume(ctx, b.runner(), cfg, cfg.Containerd.Image, source, stateVolume)
}

// validateCheckpointMetadata fails closed when the recorded scope no longer
// matches the daemon this process reaches. Exit 7 is the recorded-scope-drift
// code every container checkpoint shares.
func validateCheckpointMetadata(cfg core.Config, info daemonInfo, metadata map[string]string) error {
	if len(metadata) == 0 {
		return core.Exit(7, "containerd checkpoint is missing its recorded daemon scope; refusing the operation")
	}
	c := cfg.Containerd
	checks := []struct {
		key      string
		recorded string
		current  string
		message  string
	}{
		{checkpointMetadataHost, metadata[checkpointMetadataHost], hostScopeForConfig(cfg), "containerd host changed from %q to %q; refusing checkpoint operation"},
		{checkpointMetadataDaemon, metadata[checkpointMetadataDaemon], info.ID, "containerd daemon changed from %s to %s; refusing checkpoint operation"},
		{checkpointMetadataNamespace, metadata[checkpointMetadataNamespace], c.Namespace, "containerd namespace changed from %q to %q; refusing checkpoint operation"},
		{checkpointMetadataSnapshotter, metadata[checkpointMetadataSnapshotter], c.Snapshotter, "containerd snapshotter changed from %q to %q; refusing checkpoint operation"},
		{checkpointMetadataRuntime, metadata[checkpointMetadataRuntime], c.Runtime, "containerd runtime changed from %q to %q; refusing checkpoint operation"},
		{checkpointMetadataRootless, metadata[checkpointMetadataRootless], strconv.FormatBool(c.Rootless), "containerd rootless mode changed from %s to %s; refusing checkpoint operation"},
	}
	for _, check := range checks {
		recorded := strings.TrimSpace(check.recorded)
		if recorded == "" {
			return core.Exit(7, "containerd checkpoint is missing its recorded %s; refusing the operation", check.key)
		}
		if !strings.EqualFold(recorded, strings.TrimSpace(check.current)) {
			return core.Exit(7, check.message, recorded, strings.TrimSpace(check.current))
		}
	}
	return nil
}

// applyCheckpointLeaseConfig replays the recorded lease settings onto cfg so a
// fork or restore recreates the same container contract.
func applyCheckpointLeaseConfig(cfg *core.Config, metadata map[string]string) {
	if cfg == nil || len(metadata) == 0 {
		return
	}
	c := &cfg.Containerd
	set := func(target *string, key string) {
		if value := strings.TrimSpace(metadata[key]); value != "" {
			*target = value
		}
	}
	set(&c.Runtime, checkpointMetadataRuntime)
	set(&c.RuntimePath, checkpointMetadataRuntimePath)
	set(&c.Namespace, checkpointMetadataNamespace)
	set(&c.Snapshotter, checkpointMetadataSnapshotter)
	set(&c.Address, checkpointMetadataAddress)
	set(&c.Image, checkpointMetadataImage)
	set(&c.User, checkpointMetadataSSHUser)
	set(&c.WorkRoot, checkpointMetadataWorkRoot)
	set(&c.InitMode, checkpointMetadataInit)
	set(&c.Nested, checkpointMetadataNested)
	set(&c.GPU, checkpointMetadataGPU)
	set(&c.SeccompProfile, checkpointMetadataSeccomp)
	set(&c.Memory, checkpointMetadataMemory)
	set(&c.KrunNetwork, checkpointMetadataKrunNetwork)
	set(&c.KrunKernel, checkpointMetadataKrunKernel)
	if value, err := strconv.ParseBool(strings.TrimSpace(metadata[checkpointMetadataRootless])); err == nil {
		c.Rootless = value
	}
	if value, err := strconv.Atoi(strings.TrimSpace(metadata[checkpointMetadataCPUs])); err == nil {
		c.CPUs = value
	}
	if value, err := strconv.Atoi(strings.TrimSpace(metadata[checkpointMetadataKrunGPUFlags])); err == nil {
		c.KrunGPUFlags = value
	}
	if value, err := strconv.Atoi(strings.TrimSpace(metadata[checkpointMetadataKrunHeadroom])); err == nil {
		c.KrunRAMHeadroomMiB = value
	}
	if value, err := strconv.ParseBool(strings.TrimSpace(metadata[checkpointMetadataKrunNestedVirt])); err == nil {
		c.KrunNestedVirt = value
	}
	if value, err := strconv.ParseBool(strings.TrimSpace(metadata[checkpointMetadataDesktop])); err == nil {
		cfg.Desktop = value
	}
	if value := strings.TrimSpace(metadata[checkpointMetadataDesktopEnv]); value != "" {
		cfg.DesktopEnv = value
	}
	if value, err := strconv.ParseBool(strings.TrimSpace(metadata[checkpointMetadataBrowser])); err == nil {
		cfg.Browser = value
	}
	if value := strings.TrimSpace(metadata[checkpointMetadataNetwork]); value != "" {
		c.Network = value
	}
}

func cloneCheckpointMetadata(metadata map[string]string) map[string]string {
	out := make(map[string]string, len(metadata))
	for key, value := range metadata {
		out[key] = value
	}
	return out
}

func checkpointContainerName(server core.Server) string {
	return containercheckpoint.FirstValue(server.Name, server.Labels["container"])
}

func checkpointRequestTag(req core.NativeCheckpointCreateRequest) string {
	name := containercheckpoint.FirstValue(req.Name, req.RepoName, req.LeaseID, "checkpoint")
	return containercheckpoint.TagName(name, req.CheckpointID)
}

// checkpointContainerStatus reads the source status. A stopped source is a
// cold checkpoint: no pause/unpause and no SSH machine-id step is possible.
func checkpointContainerStatus(ctx context.Context, runner commandRunner, container string) (string, error) {
	result, err := runner.nerdctlOutput(ctx, "inspect", container, "--format", "{{.State.Status}}")
	if err != nil {
		return "", core.Exit(7, "inspect containerd container %s: %v: %s", container, err, containercheckpoint.TrimFailure(result.Stderr))
	}
	status := strings.ToLower(strings.TrimSpace(result.Stdout))
	if status == "" {
		return "", core.Exit(7, "inspect containerd container %s returned no status", container)
	}
	return status, nil
}

// checkpointRequireGNUCopy refuses an image whose cp cannot preserve the
// overlayfs xattrs (busybox cp drops trusted.overlay.*).
func checkpointRequireGNUCopy(ctx context.Context, rt core.Runtime, target core.SSHTarget) error {
	result, err := runOverSSHTarget(ctx, rt, target, "cp --version 2>&1", nil)
	if err != nil || result.ExitCode != 0 || !strings.Contains(result.Stdout+result.Stderr, "GNU coreutils") {
		return core.Exit(2, "containerd-commit checkpoints need GNU cp in the lease image; busybox cp drops trusted.overlay.* xattrs and corrupts an overlayfs state volume. Use a Debian/Ubuntu image or one with coreutils, or capture a workspace archive instead")
	}
	return nil
}

// checkpointRequireGNUCopyImage proves the same contract when the source is
// stopped and cannot be reached over SSH.
func checkpointRequireGNUCopyImage(ctx context.Context, runner commandRunner, cfg core.Config) error {
	args := append(runner.globalArgs(), "run", "--rm", "--runtime", helperRuntimePath(cfg.Containerd),
		"--entrypoint", "/bin/sh", cfg.Containerd.Image, "-c", "cp --version 2>&1")
	result, err := runner.run(ctx, args, nil, nil)
	if err != nil || result.ExitCode != 0 || !strings.Contains(result.Stdout+result.Stderr, "GNU coreutils") {
		return core.Exit(2, "containerd-commit checkpoints need GNU cp in the lease image; busybox cp drops trusted.overlay.* xattrs and corrupts an overlayfs state volume. Use a Debian/Ubuntu image or one with coreutils, or capture a workspace archive instead")
	}
	return nil
}

// checkpointEmptyMachineID returns the original machine-id and leaves the file
// empty so the committed image boots with a fresh identity.
func checkpointEmptyMachineID(ctx context.Context, rt core.Runtime, target core.SSHTarget) (string, error) {
	script := `set -eu
if [ -e /etc/machine-id ]; then
  cat /etc/machine-id
else
  printf '\000missing\000'
fi
: > /etc/machine-id
`
	result, err := runOverSSHTarget(ctx, rt, target, script, nil)
	if err != nil || result.ExitCode != 0 {
		return "", core.Exit(7, "empty /etc/machine-id over SSH: %v: %s", err, containercheckpoint.TrimFailure(result.Stderr))
	}
	return strings.TrimSpace(result.Stdout), nil
}

// checkpointRestoreMachineID restores the source's original machine-id. It is
// the finally step: it runs even when the commit failed.
func checkpointRestoreMachineID(ctx context.Context, rt core.Runtime, target core.SSHTarget, value string) error {
	if value == machineIDMissing {
		result, err := runOverSSHTarget(ctx, rt, target, "rm -f /etc/machine-id", nil)
		if err != nil || result.ExitCode != 0 {
			return core.Exit(7, "remove the checkpoint's temporary /etc/machine-id: %v: %s", err, containercheckpoint.TrimFailure(result.Stderr))
		}
		return nil
	}
	result, err := runOverSSHTarget(ctx, rt, target, "cat > /etc/machine-id", strings.NewReader(value))
	if err != nil || result.ExitCode != 0 {
		return core.Exit(7, "restore /etc/machine-id over SSH: %v: %s", err, containercheckpoint.TrimFailure(result.Stderr))
	}
	return nil
}

func checkpointPause(ctx context.Context, runner commandRunner, container string) error {
	result, err := runner.nerdctlOutput(ctx, "pause", container)
	if err != nil {
		return core.Exit(7, "nerdctl pause %s: %v: %s", container, err, containercheckpoint.TrimFailure(result.Stderr))
	}
	return nil
}

func checkpointUnpause(ctx context.Context, runner commandRunner, container string) error {
	result, err := runner.nerdctlOutput(ctx, "unpause", container)
	if err != nil {
		return core.Exit(7, "nerdctl unpause %s: %v: %s", container, err, containercheckpoint.TrimFailure(result.Stderr))
	}
	return nil
}

// checkpointCommit commits the container and returns the immutable image id.
// nerdctl requires the repository at commit time, so the tag is chosen before
// the digest exists. --change carries CMD only: nerdctl commit supports no
// LABEL directive, and the committed image inherits no lease labels.
func checkpointCommit(ctx context.Context, runner commandRunner, container, tag, change string) (string, error) {
	args := append(runner.globalArgs(), "commit", "--pause=false")
	if change != "" {
		args = append(args, "--change", change)
	}
	args = append(args, container, tag)
	result, err := runner.run(ctx, args, nil, nil)
	if err != nil {
		return "", core.Exit(7, "nerdctl commit %s: %v: %s", container, err, containercheckpoint.TrimFailure(result.Stderr))
	}
	imageID, err := containercheckpoint.ParseImageID(result.Stdout)
	if err != nil {
		return "", core.Exit(7, "nerdctl commit %s: %v", container, err)
	}
	return imageID, nil
}

// checkpointCommitCold captures a stopped container and empties machine-id in
// a derived image, because a stopped container cannot be reached over SSH.
func checkpointCommitCold(ctx context.Context, b *backend, cfg core.Config, container, tag string) (string, error) {
	runner := b.runner()
	if err := checkpointRequireGNUCopyImage(ctx, runner, cfg); err != nil {
		return "", err
	}
	sourceTag := tag + "-source"
	sourceID, err := checkpointCommit(ctx, runner, container, sourceTag, "")
	if err != nil {
		return "", err
	}
	helperName := tag + "-mid"
	runArgs := append(runner.globalArgs(), "run", "--name", helperName, "--runtime", helperRuntimePath(cfg.Containerd),
		"--entrypoint", "/bin/sh", sourceID, "-c", ": > /etc/machine-id")
	if result, err := runner.run(ctx, runArgs, nil, nil); err != nil {
		cleanupCtx, cancel := checkpointCleanupContext(ctx)
		defer cancel()
		_, _ = runner.nerdctlOutput(cleanupCtx, "rmi", "-f", sourceID)
		return "", core.Exit(7, "prepare the cold checkpoint machine-id: %v: %s", err, containercheckpoint.TrimFailure(result.Stderr))
	}
	finalID, err := checkpointCommit(ctx, runner, helperName, tag, containercheckpoint.BootableCommandChange())
	cleanupCtx, cancel := checkpointCleanupContext(ctx)
	defer cancel()
	_, _ = runner.nerdctlOutput(cleanupCtx, "rm", "-f", helperName)
	_, _ = runner.nerdctlOutput(cleanupCtx, "rmi", "-f", sourceID)
	if err != nil {
		return "", err
	}
	return finalID, nil
}

// checkpointCloneVolume copies a captured state volume into a fresh volume. A
// rootful helper needs --privileged to preserve trusted.overlay.* xattrs;
// krun cannot be the copy runtime, so the base crun engine is used.
func checkpointCloneVolume(ctx context.Context, runner commandRunner, cfg core.Config, image, source, destination string) error {
	if strings.TrimSpace(destination) == "" {
		return core.Exit(2, "containerd checkpoint state volume has no destination name")
	}
	if err := checkpointRemoveVolume(ctx, runner, destination); err != nil {
		return err
	}
	args := append(runner.globalArgs(), "run", "--rm", "--runtime", helperRuntimePath(cfg.Containerd))
	if !cfg.Containerd.Rootless {
		args = append(args, "--privileged")
	}
	args = append(args,
		"-v", source+":/src:ro",
		"-v", destination+":/dst",
		"--entrypoint", "/bin/sh", image, "-c", "cp -a /src/. /dst/",
	)
	result, err := runner.run(ctx, args, nil, nil)
	if err != nil {
		return core.Exit(7, "copy the state volume %s to %s: %v: %s", source, destination, err, containercheckpoint.TrimFailure(result.Stderr))
	}
	return nil
}

func checkpointRemoveVolume(ctx context.Context, runner commandRunner, volume string) error {
	result, err := runner.nerdctlOutput(ctx, "volume", "rm", "--force", volume)
	if err != nil && !strings.Contains(strings.ToLower(result.Stderr+err.Error()), "no such volume") {
		return shared.LocalCommandError("nerdctl volume rm", result, err)
	}
	return nil
}

func checkpointRemoveContainer(ctx context.Context, runner commandRunner, container string) error {
	result, err := runner.nerdctlOutput(ctx, "rm", "-f", container)
	if err != nil && !containerNotFound(result) {
		return shared.LocalCommandError("nerdctl rm", result, err)
	}
	return nil
}

func checkpointVolumeExists(ctx context.Context, runner commandRunner, volume string) (bool, error) {
	result, err := runner.nerdctlOutput(ctx, "volume", "ls", "--format", "{{.Name}}")
	if err != nil {
		return false, shared.LocalCommandError("nerdctl volume ls", result, err)
	}
	for _, line := range strings.Split(result.Stdout, "\n") {
		if strings.TrimSpace(line) == volume {
			return true, nil
		}
	}
	return false, nil
}

func checkpointInspectImage(ctx context.Context, runner commandRunner, name string) (string, bool, error) {
	if strings.TrimSpace(name) == "" {
		return "", false, core.Exit(7, "containerd checkpoint has no recorded image tag")
	}
	result, err := runner.nerdctlOutput(ctx, "image", "inspect", name, "--format", "{{.Id}}")
	if err != nil {
		detail := containercheckpoint.TrimFailure(result.Stderr)
		if containercheckpoint.InspectReportsMissing(detail + " " + result.Stdout + " " + err.Error()) {
			return "", true, nil
		}
		return "", false, core.Exit(7, "containerd image inspect %s: %v: %s", name, err, detail)
	}
	imageID, err := containercheckpoint.ParseImageID(result.Stdout)
	if err != nil {
		return "", false, core.Exit(7, "containerd image inspect %s: %v", name, err)
	}
	return imageID, false, nil
}

// checkpointRecreatePorts reuses the exact published mappings. Re-allocating
// would change the claim's SSH port and silently drop the lease's exposures.
func checkpointRecreatePorts(ctx context.Context, runner commandRunner, container string, metadata map[string]string) (string, []string, error) {
	result, err := runner.nerdctlOutput(ctx, "port", container)
	if err == nil {
		hostPort, extra := parseCheckpointPorts(result.Stdout)
		if hostPort != "" {
			return hostPort, extra, nil
		}
	}
	hostPort := strings.TrimSpace(metadata[checkpointMetadataPublishedPort])
	if hostPort == "" {
		return "", nil, core.Exit(7, "containerd checkpoint restore cannot determine the lease's published SSH port: %v: %s", err, containercheckpoint.TrimFailure(result.Stderr))
	}
	extra := []string{}
	if encoded := strings.TrimSpace(metadata[checkpointMetadataExtraPorts]); encoded != "" {
		if decodeErr := json.Unmarshal([]byte(encoded), &extra); decodeErr != nil {
			return "", nil, core.Exit(7, "decode the recorded extra port mappings: %v", decodeErr)
		}
	}
	return hostPort, extra, nil
}

// parseCheckpointPorts converts `nerdctl port` lines into the SSH host port
// and the remaining `-p host:container` specs.
func parseCheckpointPorts(output string) (string, []string) {
	hostPort := ""
	extra := []string{}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " -> ", 2)
		if len(parts) != 2 {
			continue
		}
		containerPort := strings.TrimSuffix(strings.TrimSpace(parts[0]), "/tcp")
		hostEndpoint := strings.TrimSpace(parts[1])
		lastColon := strings.LastIndex(hostEndpoint, ":")
		if lastColon < 0 {
			continue
		}
		host := hostEndpoint[:lastColon]
		port := hostEndpoint[lastColon+1:]
		if containerPort == "" || port == "" {
			continue
		}
		spec := host + ":" + port + ":" + containerPort
		if containerPort == defaultSSHPort && hostPort == "" {
			hostPort = port
			continue
		}
		extra = append(extra, spec)
	}
	return hostPort, extra
}

func helperRuntimePath(c core.ContainerdConfig) string {
	if c.Runtime == "krun" {
		// A krun microVM cannot run the short-lived copy helper cheaply; krun is
		// a crun handler, so the base engine runs the same image.
		return "/usr/bin/crun"
	}
	if path := strings.TrimSpace(c.RuntimePath); path != "" {
		return path
	}
	return "/usr/bin/" + c.Runtime
}

// runtimePathForConfig is the lease's own runtime binary, recorded so a fork
// or restore reproduces it instead of substituting the helper engine.
func runtimePathForConfig(c core.ContainerdConfig) string {
	if path := strings.TrimSpace(c.RuntimePath); path != "" {
		return path
	}
	return "/usr/bin/" + c.Runtime
}

func firstCheckpointLabelBool(label string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(label)) {
	case "1", "true", "yes":
		return true
	case "0", "false", "no":
		return false
	default:
		return fallback
	}
}
