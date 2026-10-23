package containerd

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

type backend struct {
	spec core.ProviderSpec
	cfg  core.Config
	rt   core.Runtime

	mu       sync.Mutex
	forwards map[string]*sshForward
	acquired map[string]core.LeaseTarget
}

type sshForward struct {
	cancel    context.CancelFunc
	localPort string
}

func newBackend(spec core.ProviderSpec, cfg core.Config, rt core.Runtime) core.Backend {
	cfg.Provider = providerName
	return &backend{spec: spec, cfg: cfg, rt: rt, forwards: map[string]*sshForward{}, acquired: map[string]core.LeaseTarget{}}
}

func (b *backend) Spec() core.ProviderSpec { return b.spec }

func (b *backend) runner() commandRunner { return commandRunner{cfg: b.cfg, rt: b.rt} }

// Acquire creates one containerd container, publishes its SSH port on the
// operator's loopback, verifies the daemon and the SSH contract, and publishes
// the exact local claim.
func (b *backend) Acquire(ctx context.Context, req core.AcquireRequest) (core.LeaseTarget, error) {
	cfg := b.cfg
	if requested := strings.TrimSpace(req.RequestedLeaseID); requested != "" {
		return core.LeaseTarget{}, core.Exit(2, "provider=%s does not support caller-supplied lease IDs; omit --lease-id", providerName)
	}
	scope := Provider{}.ClaimScope(cfg)
	if strings.TrimSpace(scope) == "" {
		return core.LeaseTarget{}, core.Exit(2, "containerd runtime scope is incomplete; refusing to create an unscoped lease")
	}
	info, err := b.verifyDaemon(ctx)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if err := b.refuseUnsupportedImageProfile(ctx); err != nil {
		return core.LeaseTarget{}, err
	}
	servers, err := b.listContainers(ctx)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	leaseID := core.NewLeaseID()
	slug, err := core.AllocateDirectLeaseSlug(leaseID, req.RequestedSlug, servers)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if cfg.Containerd.CRIU == "auto" {
		fmt.Fprintf(b.rt.Stderr, "criu=auto narrowed to off for this lease: automatic CRIU requires the checkpoint seam's pre-flight probe; set containerd.criu: in-container or run `crabbox doctor --provider containerd` to inspect the host\n")
	}
	keyPath, publicKey, err := core.EnsureTestboxKeyForConfig(cfg, leaseID)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	cleanupKey := true
	defer func() {
		if cleanupKey {
			core.RemoveStoredTestboxKey(leaseID)
		}
	}()
	cfg.SSHKey = keyPath
	inputsDir, err := b.prepareInputs(ctx, leaseID, publicKey)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	cleanupInputs := true
	defer func() {
		if cleanupInputs {
			_ = b.removeInputs(context.WithoutCancel(ctx), inputsDir.dir)
		}
	}()
	hostPort, err := b.allocatePort(ctx)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	var extraPorts []string
	for _, containerPort := range cfg.ExposedPorts {
		published, err := b.allocatePort(ctx)
		if err != nil {
			return core.LeaseTarget{}, err
		}
		extraPorts = append(extraPorts, "127.0.0.1:"+published+":"+containerPort)
	}
	name := core.LeaseProviderName(leaseID, slug)
	stateVolume := ""
	if cfg.Containerd.StateVolume == "auto" {
		stateVolume = "crabbox-" + leaseID + "-state"
	}
	args, err := b.buildRunArgs(ctx, leaseID, slug, name, inputsDir.dir, stateVolume, hostPort, extraPorts, req.Keep, info)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	fmt.Fprintf(b.rt.Stderr, "provisioning provider=%s lease=%s slug=%s namespace=%s runtime=%s image=%s keep=%v\n",
		providerName, leaseID, slug, cfg.Containerd.Namespace, cfg.Containerd.Runtime, cfg.Containerd.Image, req.Keep)
	rollback := func() {
		b.stopForward(leaseID)
		_ = b.removeContainer(context.WithoutCancel(ctx), name)
		if stateVolume != "" {
			_ = b.removeVolume(context.WithoutCancel(ctx), stateVolume)
		}
	}
	result, err := b.runner().run(ctx, args, nil, b.rt.Stderr)
	containerID := strings.TrimSpace(result.Stdout)
	if err != nil || containerID == "" {
		// nerdctl creates the named state volume before the container starts, so a
		// failed start leaves the volume behind unless the same rollback that
		// covers the later stages runs here too.
		rollback()
		if err == nil {
			err = core.Exit(2, "nerdctl run did not return a container id")
		}
		if reason, ok := portmapUnavailable(result); ok {
			return core.LeaseTarget{}, core.Exit(2, "containerd could not publish the lease SSH port 127.0.0.1:%s: %s; run the lease on a host with the kernel xt_multiport extension (opencharly/layer-containerd#5) or on a network that does not publish ports", hostPort, reason)
		}
		return core.LeaseTarget{}, shared.LocalCommandError("containerd run", result, err)
	}
	if err := b.waitForPort(ctx, hostPort); err != nil {
		rollback()
		return core.LeaseTarget{}, err
	}
	target, err := b.sshTargetForPort(ctx, leaseID, hostPort)
	if err != nil {
		rollback()
		return core.LeaseTarget{}, err
	}
	if err := b.startForward(ctx, leaseID, hostPort, &target); err != nil {
		rollback()
		return core.LeaseTarget{}, err
	}
	if err := core.WaitForSSHReady(ctx, &target, b.rt.Stderr, "containerd ssh", core.BootstrapWaitTimeout(cfg)); err != nil {
		rollback()
		return core.LeaseTarget{}, err
	}
	if err := b.runReadinessHook(ctx, leaseID, target); err != nil {
		rollback()
		return core.LeaseTarget{}, err
	}
	if cfg.Containerd.SeccompProfile != "" && (cfg.Browser || cfg.Desktop) {
		if !b.hostPathExists(ctx, cfg.Containerd.SeccompProfile) {
			rollback()
			return core.LeaseTarget{}, core.Exit(2, "containerd.seccompProfile %s does not exist on the containerd host; a stock seccomp profile breaks Chromium's sandbox, so install the narrow profile from CONTAINERD.md §3.1 (generate it from the installed DefaultProfile() with unshare/clone allowed, keyctl/mount EPERM) and retry", cfg.Containerd.SeccompProfile)
		}
	}
	labels := b.leaseLabels(leaseID, slug, info, stateVolume, inputsDir.dir, req.Keep)
	labels["published_port"] = hostPort
	labels["container"] = name
	server := core.Server{
		CloudID: containerID,
		Name:    name,
		Status:  "ready",
		Labels:  labels,
	}
	claim, err := core.ClaimLeaseTargetForRepoConfigScopeIfUnchangedDurable(
		leaseID, slug, cfg, scope, server, target, req.Repo.Root, cfg.IdleTimeout, req.Reclaim, core.LeaseClaim{}, false,
	)
	if err != nil {
		rollback()
		return core.LeaseTarget{}, err
	}
	if claim.LeaseID != leaseID {
		rollback()
		return core.LeaseTarget{}, core.Exit(2, "containerd lease claim was not persisted for lease=%s", leaseID)
	}
	core.SetServerLeaseClaimSnapshot(&server, claim, true)
	cleanupInputs, cleanupKey = false, false
	lease := core.LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}
	b.remember(lease)
	fmt.Fprintf(b.rt.Stderr, "provisioned lease=%s container=%s state=ready\n", leaseID, shortID(containerID))
	return lease, nil
}

// Resolve rebuilds a lease from its exact local claim and verifies the live
// daemon identity before any credentialed work.
func (b *backend) Resolve(ctx context.Context, req core.ResolveRequest) (core.LeaseTarget, error) {
	if strings.TrimSpace(req.ID) != "" {
		if lease, ok := b.remembered(req.ID); ok {
			return lease, nil
		}
	}
	scope := Provider{}.ClaimScope(b.cfg)
	if strings.TrimSpace(scope) == "" {
		return core.LeaseTarget{}, core.Exit(2, "containerd runtime scope is incomplete; refusing to resolve an unscoped lease")
	}
	claim, ok, err := shared.ResolveProviderClaimStrict(req.ID, providerName, scope)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if !ok {
		return core.LeaseTarget{}, core.Exit(4, "containerd lease not found: %s", req.ID)
	}
	if req.Repo.Root != "" {
		if err := core.CheckLeaseClaimRepositoryOwner(claim.LeaseID, claim, req.Repo.Root, req.Reclaim); err != nil {
			return core.LeaseTarget{}, err
		}
	}
	lease, err := b.leaseFromClaim(ctx, claim, req.Prepare)
	if err != nil {
		return core.LeaseTarget{}, err
	}
	if !req.ReleaseOnly && !req.StatusOnly {
		info, err := b.verifyDaemon(ctx)
		if err != nil {
			return core.LeaseTarget{}, err
		}
		if err := validateClaimDaemon(claim, info, b.cfg); err != nil {
			return core.LeaseTarget{}, err
		}
	}
	if req.Prepare {
		if err := core.WaitForSSHReady(ctx, &lease.SSH, b.rt.Stderr, "containerd ssh", core.BootstrapWaitTimeout(b.cfg)); err != nil {
			return core.LeaseTarget{}, err
		}
	}
	return lease, nil
}

func (b *backend) leaseFromClaim(ctx context.Context, claim core.LeaseClaim, prepare bool) (core.LeaseTarget, error) {
	server := core.Server{CloudID: claim.CloudID, Name: claim.Labels["container"], Status: claim.Labels["state"], Labels: shared.CloneLabels(claim.Labels)}
	if server.Name == "" {
		server.Name = core.LeaseProviderName(claim.LeaseID, claim.Slug)
	}
	target := core.SSHTargetFromConfig(b.cfg, "127.0.0.1")
	target.TargetOS = core.TargetLinux
	target.Port = strconv.Itoa(claim.SSHPort)
	target.User = b.cfg.Containerd.User
	target.Key = ""
	if err := core.UseStoredTestboxKey(&target, claim.LeaseID); err != nil {
		return core.LeaseTarget{}, err
	}
	if b.runner().remote() {
		published := strings.TrimSpace(claim.Labels["published_port"])
		if published == "" {
			return core.LeaseTarget{}, core.Exit(4, "containerd lease %s has no published port; refusing to resolve an unscoped remote lease", claim.LeaseID)
		}
		if prepare {
			if err := b.startForward(ctx, claim.LeaseID, published, &target); err != nil {
				return core.LeaseTarget{}, err
			}
		}
	}
	lease := core.LeaseTarget{Server: server, SSH: target, LeaseID: claim.LeaseID}
	core.SetServerLeaseClaimSnapshot(&server, claim, true)
	lease.Server = server
	return lease, nil
}

func validateClaimDaemon(claim core.LeaseClaim, info daemonInfo, cfg core.Config) error {
	if recorded := strings.TrimSpace(claim.Labels["daemon"]); recorded != "" && recorded != info.ID {
		return core.Exit(4, "containerd lease %s was created on daemon %s but the configured socket now reaches daemon %s; refusing to reuse a lease on a different daemon", claim.LeaseID, recorded, info.ID)
	}
	if recorded := strings.TrimSpace(claim.Labels["namespace"]); recorded != "" && recorded != cfg.Containerd.Namespace {
		return core.Exit(4, "containerd lease %s belongs to namespace %q but containerd.namespace is %q", claim.LeaseID, recorded, cfg.Containerd.Namespace)
	}
	if recorded := strings.TrimSpace(claim.Labels["host"]); recorded != "" && recorded != hostScopeForConfig(cfg) {
		return core.Exit(4, "containerd lease %s was created on host %q but the provider is configured for %q", claim.LeaseID, recorded, hostScopeForConfig(cfg))
	}
	return nil
}

// List returns normalized lease views without printing.
func (b *backend) List(ctx context.Context, req core.ListRequest) ([]core.LeaseView, error) {
	containers, err := b.listContainers(ctx)
	if err != nil {
		return nil, err
	}
	return containers, nil
}

// listContainers reads the label-filtered inventory for this provider scope.
// Labels come from `inspect`, not `ps`, because a label value may itself contain
// commas and the `ps` label string cannot be split unambiguously.
func (b *backend) listContainers(ctx context.Context) ([]core.LeaseView, error) {
	result, err := b.runner().nerdctlOutput(ctx, "ps", "-a",
		"--filter", "label=crabbox=true",
		"--filter", "label=provider="+providerName,
		"--format", "{{.ID}}",
	)
	if err != nil {
		return nil, shared.LocalCommandError("nerdctl ps", result, err)
	}
	host := hostScopeForConfig(b.cfg)
	namespace := b.cfg.Containerd.Namespace
	views := []core.LeaseView{}
	for _, line := range strings.Split(result.Stdout, "\n") {
		id := strings.TrimSpace(line)
		if id == "" {
			continue
		}
		// nerdctl's default `inspect` shape is a JSON array. The `--format
		// '{{json .}}'` form emits one object per line, which is a different
		// contract and cannot be decoded into the array below.
		inspect, err := b.runner().nerdctlOutput(ctx, "inspect", id)
		if err != nil {
			return nil, shared.LocalCommandError("nerdctl inspect", inspect, err)
		}
		var observed []struct {
			ID    string `json:"Id"`
			Name  string `json:"Name"`
			State struct {
				Status string `json:"Status"`
			} `json:"State"`
			Config struct {
				Image  string            `json:"Image"`
				Labels map[string]string `json:"Labels"`
			} `json:"Config"`
		}
		if err := json.Unmarshal([]byte(inspect.Stdout), &observed); err != nil || len(observed) == 0 {
			return nil, core.Exit(5, "nerdctl inspect returned invalid JSON for %s: %v", id, err)
		}
		row := observed[0]
		labels := row.Config.Labels
		if labels["provider"] != providerName {
			continue
		}
		if labels["host"] != "" && labels["host"] != host {
			continue
		}
		if labels["namespace"] != "" && labels["namespace"] != namespace {
			continue
		}
		name := strings.TrimPrefix(row.Name, "/")
		state := labels["state"]
		if state == "" {
			state = row.State.Status
		}
		if state == "" {
			state = "unknown"
		}
		views = append(views, core.LeaseView{
			CloudID: row.ID,
			Name:    name,
			Status:  state,
			Labels:  labels,
		})
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Name < views[j].Name })
	return views, nil
}

func (b *backend) Touch(ctx context.Context, req core.TouchRequest) (core.Server, error) {
	scope := Provider{}.ClaimScope(b.cfg)
	updated, err := shared.CommitClaimTouch(ctx, req, shared.ClaimTouchPolicy{
		Provider: providerName,
		Authorize: func(_ context.Context, lease core.LeaseTarget, claim core.LeaseClaim) error {
			if claim.Provider != providerName {
				return core.Exit(4, "containerd lease %s claim belongs to provider %q", lease.LeaseID, claim.Provider)
			}
			if claim.ProviderScope != scope {
				return core.Exit(4, "containerd lease %s claim scope changed; refusing touch", lease.LeaseID)
			}
			for _, key := range []string{"daemon", "namespace", "host"} {
				recorded := strings.TrimSpace(claim.Labels[key])
				want := strings.TrimSpace(lease.Server.Labels[key])
				if key == "host" {
					want = hostScopeForConfig(b.cfg)
				}
				if key == "namespace" {
					want = b.cfg.Containerd.Namespace
				}
				if recorded != "" && want != "" && recorded != want {
					return core.Exit(4, "containerd lease %s %s=%q does not match %q", lease.LeaseID, key, recorded, want)
				}
			}
			return nil
		},
		Prepare: func(expected core.LeaseClaim) (map[string]string, time.Time) {
			now := b.now()
			return core.TouchDirectLeaseLabelsWithIdleTimeoutOverride(expected.Labels, b.cfg, req.State, now, req.IdleTimeoutOverride), now
		},
	})
	if err != nil {
		return core.Server{}, err
	}
	server := req.Lease.Server
	server.Labels = updated.Labels
	if state := strings.TrimSpace(server.Labels["state"]); state != "" {
		server.Status = state
	}
	core.SetServerLeaseClaimSnapshot(&server, updated, true)
	b.remember(core.LeaseTarget{Server: server, SSH: req.Lease.SSH, LeaseID: req.Lease.LeaseID})
	return server, nil
}

// ReleaseLease ends SSH sessions, stops the container, removes the owned state
// volume, inputs and key, verifies the container is gone by listing, and then
// removes the exact local claim.
func (b *backend) ReleaseLease(ctx context.Context, req core.ReleaseLeaseRequest) error {
	lease := req.Lease
	b.stopForward(lease.LeaseID)
	claim, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
	if err != nil {
		return err
	}
	if info, err := b.verifyDaemon(ctx); err != nil {
		return err
	} else if recorded := strings.TrimSpace(claim.Labels["daemon"]); recorded != "" && recorded != info.ID {
		return core.Exit(4, "containerd lease %s was created on daemon %s but the configured socket now reaches daemon %s; refusing to release a same-named container on a different daemon", lease.LeaseID, recorded, info.ID)
	}
	name := strings.TrimSpace(lease.Server.Name)
	if name == "" {
		name = strings.TrimSpace(claim.Labels["container"])
	}
	if name == "" {
		name = core.LeaseProviderName(lease.LeaseID, lease.Server.Labels["slug"])
	}
	if err := b.stopContainer(ctx, lease, name); err != nil {
		return err
	}
	if err := b.removeContainer(ctx, name); err != nil {
		return err
	}
	present, err := b.containerListed(ctx, name)
	if err != nil {
		return err
	}
	if present {
		return core.Exit(5, "container %s is still listed after `nerdctl rm -f` exited 0 (nerdctl remove.go:88 reports success on a failed removal); the lease claim is retained so a retry stays authorized", name)
	}
	if volume := strings.TrimSpace(lease.Server.Labels["state_volume"]); volume != "" {
		if err := b.removeVolume(ctx, volume); err != nil {
			return err
		}
	}
	if err := b.removeInputs(ctx, lease.Server.Labels["inputs_dir"]); err != nil {
		return err
	}
	core.RemoveStoredTestboxKey(lease.LeaseID)
	if exists {
		if err := core.RemoveLeaseClaimIfUnchanged(lease.LeaseID, claim); err != nil {
			return err
		}
	}
	b.forget(lease.LeaseID)
	fmt.Fprintf(b.rt.Stderr, "released containerd lease=%s container=%s\n", lease.LeaseID, name)
	return nil
}

func (b *backend) stopContainer(ctx context.Context, lease core.LeaseTarget, name string) error {
	c := b.cfg.Containerd
	if c.InitMode == "systemd" && c.Runtime == "krun" {
		// A krun systemd guest powers off through systemctl; never signal it and
		// never read a KRUN_INIT_PID1 exit code (it is always 0).
		_ = b.runOverLeaseSSH(ctx, lease, "systemctl poweroff")
		return nil
	}
	args := []string{"stop", "--time", "30"}
	if c.Runtime == "krun" && c.InitMode == "direct" {
		args = append(args, "--signal", "SIGINT")
	}
	args = append(args, name)
	result, err := b.runner().nerdctlOutput(ctx, args...)
	if err != nil && !containerNotFound(result) {
		return shared.LocalCommandError("nerdctl stop", result, err)
	}
	return nil
}

func (b *backend) removeContainer(ctx context.Context, name string) error {
	result, err := b.runner().nerdctlOutput(ctx, "rm", "-f", name)
	if err != nil && !containerNotFound(result) {
		return shared.LocalCommandError("nerdctl rm", result, err)
	}
	return nil
}

func (b *backend) containerListed(ctx context.Context, name string) (bool, error) {
	result, err := b.runner().nerdctlOutput(ctx, "ps", "-a", "--filter", "name=^"+name+"$", "--format", "{{.Names}}")
	if err != nil {
		return false, shared.LocalCommandError("nerdctl ps", result, err)
	}
	for _, line := range strings.Split(result.Stdout, "\n") {
		if strings.TrimSpace(line) == name {
			return true, nil
		}
	}
	return false, nil
}

func (b *backend) removeVolume(ctx context.Context, volume string) error {
	result, err := b.runner().nerdctlOutput(ctx, "volume", "rm", "--force", volume)
	if err != nil && !strings.Contains(strings.ToLower(result.Stderr+err.Error()), "no such volume") {
		return shared.LocalCommandError("nerdctl volume rm", result, err)
	}
	return nil
}

func containerNotFound(result core.LocalCommandResult) bool {
	text := strings.ToLower(result.Stderr + result.Stdout)
	return strings.Contains(text, "no such container") || strings.Contains(text, "not found")
}

// portmapUnavailable recognises the CNI portmap failure that a host without the
// kernel xt_multiport extension produces, so the error names the host
// prerequisite instead of a wall of nerdctl output.
func portmapUnavailable(result core.LocalCommandResult) (string, bool) {
	text := result.Stderr + "\n" + result.Stdout
	switch {
	case strings.Contains(text, "multiport"):
		return "the CNI portmap plugin needs the xt_multiport kernel extension, which this host lacks", true
	case strings.Contains(text, "CNI-HOSTPORT-DNAT"):
		return "the CNI portmap plugin could not install the host DNAT rule", true
	}
	return "", false
}

// Cleanup deletes claims first, then proves and deletes matched orphans.
func (b *backend) Cleanup(ctx context.Context, req core.CleanupRequest) error {
	scope := Provider{}.ClaimScope(b.cfg)
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return err
	}
	claimed := map[string]bool{}
	host, namespace := hostScopeForConfig(b.cfg), b.cfg.Containerd.Namespace
	for _, claim := range claims {
		if claim.Provider != providerName {
			continue
		}
		if recordedHost := claim.Labels["host"]; recordedHost != "" && recordedHost != host {
			continue
		}
		if recordedNamespace := claim.Labels["namespace"]; recordedNamespace != "" && recordedNamespace != namespace {
			continue
		}
		// A claim on this host/namespace is never an orphan, even when its
		// runtime scope belongs to a different provider configuration.
		claimed[claim.LeaseID] = true
		if claim.ProviderScope != scope {
			continue
		}
		if req.DryRun {
			fmt.Fprintf(b.rt.Stderr, "cleanup dry-run: would release containerd lease=%s container=%s\n", claim.LeaseID, claim.Labels["container"])
			continue
		}
		name := claim.Labels["container"]
		if name == "" {
			name = core.LeaseProviderName(claim.LeaseID, claim.Slug)
		}
		if err := b.removeContainer(ctx, name); err != nil {
			return err
		}
		if volume := claim.Labels["state_volume"]; volume != "" {
			if err := b.removeVolume(ctx, volume); err != nil {
				return err
			}
		}
		if err := b.removeInputs(ctx, claim.Labels["inputs_dir"]); err != nil {
			return err
		}
		core.RemoveStoredTestboxKey(claim.LeaseID)
		if err := core.RemoveLeaseClaimIfUnchanged(claim.LeaseID, claim); err != nil {
			return err
		}
		fmt.Fprintf(b.rt.Stderr, "cleanup: released containerd lease=%s container=%s\n", claim.LeaseID, name)
	}
	views, err := b.listContainers(ctx)
	if err != nil {
		return err
	}
	for _, view := range views {
		leaseID := view.Labels["lease"]
		if leaseID == "" || claimed[leaseID] {
			continue
		}
		// A labeled container with a lease ID but no local claim is an orphan of
		// this provider scope; it is proven by the label filter above.
		if req.DryRun {
			fmt.Fprintf(b.rt.Stderr, "cleanup dry-run: would delete orphan containerd container=%s lease=%s\n", view.Name, leaseID)
			continue
		}
		if err := b.removeContainer(ctx, view.Name); err != nil {
			return err
		}
		// An orphan carries the same provider-owned resources a claim does; a
		// container-only delete leaks its state volume, inputs and lease key.
		if volume := strings.TrimSpace(view.Labels["state_volume"]); volume != "" {
			if err := b.removeVolume(ctx, volume); err != nil {
				return err
			}
		}
		if err := b.removeInputs(ctx, view.Labels["inputs_dir"]); err != nil {
			return err
		}
		core.RemoveStoredTestboxKey(leaseID)
		fmt.Fprintf(b.rt.Stderr, "cleanup: deleted orphan containerd container=%s lease=%s\n", view.Name, leaseID)
	}
	return nil
}

// Pause and Resume free and restore remote compute without losing state.
func (b *backend) Pause(ctx context.Context, req core.PauseRequest) error {
	return b.pauseResume(ctx, req.ID, "pause")
}

func (b *backend) Resume(ctx context.Context, req core.ResumeRequest) error {
	return b.pauseResume(ctx, req.ID, "unpause")
}

func (b *backend) pauseResume(ctx context.Context, id, verb string) error {
	lease, err := b.Resolve(ctx, core.ResolveRequest{ID: id})
	if err != nil {
		return err
	}
	result, err := b.runner().nerdctlOutput(ctx, verb, lease.Server.Name)
	if err != nil {
		return shared.LocalCommandError("nerdctl "+verb, result, err)
	}
	return nil
}

// ResolveExecLeaseUnderClaim prepares access for exec while core holds the
// shared claim fence. It verifies the claim and never publishes a claim.
func (b *backend) ResolveExecLeaseUnderClaim(ctx context.Context, req core.ResolveRequest, claim core.LeaseClaim) (core.LeaseTarget, error) {
	if claim.Provider != providerName {
		return core.LeaseTarget{}, core.Exit(4, "containerd lease %s claim belongs to provider %q", claim.LeaseID, claim.Provider)
	}
	scope := Provider{}.ClaimScope(b.cfg)
	if claim.ProviderScope != scope {
		return core.LeaseTarget{}, core.Exit(4, "containerd lease %s claim scope does not match the configured daemon", claim.LeaseID)
	}
	if err := b.validateClaimTarget(ctx, claim); err != nil {
		return core.LeaseTarget{}, err
	}
	return b.leaseFromClaim(ctx, claim, true)
}

func (b *backend) validateClaimTarget(ctx context.Context, claim core.LeaseClaim) error {
	info, err := b.verifyDaemon(ctx)
	if err != nil {
		return err
	}
	return validateClaimDaemon(claim, info, b.cfg)
}

// StopForRepository validates the calling repository under the release fence
// before any native cleanup.
func (b *backend) StopForRepository(ctx context.Context, req core.StopRequest, repoRoot string) error {
	lease, err := b.Resolve(ctx, core.ResolveRequest{ID: req.ID, ReleaseOnly: true, Repo: core.Repo{Root: repoRoot}})
	if err != nil {
		return err
	}
	return b.ReleaseLease(ctx, core.ReleaseLeaseRequest{Lease: lease})
}

// UpdateTailscaleMetadata reads the in-lease tailscale state files and records
// them on the claim labels. It never runs `tailscale up`.
func (b *backend) UpdateTailscaleMetadata(ctx context.Context, lease core.LeaseTarget, meta core.TailscaleMetadata) (core.Server, error) {
	server := lease.Server
	server.Labels = shared.CloneLabels(server.Labels)
	if meta.Enabled {
		server.Labels["tailscale"] = "true"
	}
	if meta.State != "" {
		server.Labels["tailscale_state"] = meta.State
	}
	if meta.FQDN != "" {
		server.Labels["tailscale_fqdn"] = meta.FQDN
	}
	if meta.IPv4 != "" {
		server.Labels["tailscale_ipv4"] = meta.IPv4
	}
	if meta.Hostname != "" {
		server.Labels["tailscale_hostname"] = meta.Hostname
	}
	claim, err := shared.CommitClaimTouch(ctx, core.TouchRequest{Lease: core.LeaseTarget{Server: server, SSH: lease.SSH, LeaseID: lease.LeaseID}}, shared.ClaimTouchPolicy{
		Provider: providerName,
		Authorize: func(_ context.Context, _ core.LeaseTarget, claim core.LeaseClaim) error {
			if claim.Provider != providerName {
				return core.Exit(4, "containerd lease %s claim belongs to provider %q", lease.LeaseID, claim.Provider)
			}
			return nil
		},
		Prepare: func(core.LeaseClaim) (map[string]string, time.Time) { return server.Labels, b.now() },
	})
	if err != nil {
		return core.Server{}, err
	}
	server.Labels = claim.Labels
	core.SetServerLeaseClaimSnapshot(&server, claim, true)
	b.remember(core.LeaseTarget{Server: server, SSH: lease.SSH, LeaseID: lease.LeaseID})
	return server, nil
}

// Ports reads the published mappings. Adding a mapping to a running container
// is not possible with nerdctl, so publish/unpublish name the working
// configuration instead of silently doing nothing.
func (b *backend) Ports(ctx context.Context, req core.PortsRequest) (string, error) {
	if len(req.Publish) > 0 || len(req.Unpublish) > 0 {
		return "", core.Exit(2, "nerdctl cannot add or remove a published port on a running container; create the lease with `--expose <port>` (repeatable) and the provider publishes each port on loopback at creation")
	}
	lease, err := b.Resolve(ctx, core.ResolveRequest{ID: req.ID})
	if err != nil {
		return "", err
	}
	result, err := b.runner().nerdctlOutput(ctx, "port", lease.Server.Name)
	if err != nil {
		return "", shared.LocalCommandError("nerdctl port", result, err)
	}
	lines := []string{}
	for _, line := range strings.Split(result.Stdout, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	if req.JSON {
		return mustJSON(lines), nil
	}
	return strings.Join(lines, "\n"), nil
}

// Doctor reports daemon, runtime and host readiness. Every check is read-only.
func (b *backend) Doctor(ctx context.Context, req core.DoctorRequest) (core.DoctorResult, error) {
	runner := b.runner()
	checks := []core.DoctorCheck{}
	add := func(status, check, message string) {
		checks = append(checks, core.DoctorCheck{Status: status, Check: check, Message: message})
	}

	if result, err := runner.run(ctx, []string{"--version"}, nil, nil); err != nil || result.ExitCode != 0 {
		add("failed", "nerdctl", "nerdctl is not available on the containerd host; install nerdctl (layer-nerdctl) or use provider=local-container")
		return core.DoctorResult{Provider: providerName, Checks: checks}, nil
	} else {
		add("ok", "nerdctl", strings.TrimSpace(result.Stdout))
	}

	info, err := b.verifyDaemon(ctx)
	if err != nil {
		add("failed", "daemon", err.Error())
		return core.DoctorResult{Provider: providerName, Checks: checks}, nil
	}
	mode := "rootful"
	if info.rootless() {
		mode = "rootless"
	}
	add("ok", "daemon", "id="+info.ID+" mode="+mode+" namespace="+b.cfg.Containerd.Namespace)
	add("ok", "namespace", "namespace="+b.cfg.Containerd.Namespace+" exists=true")

	if result, err := b.runner().runShell(ctx, "test -x "+core.ShellQuote(b.runtimePath()), nil, nil); err == nil && result.ExitCode == 0 {
		add("ok", "runtime", "path="+b.runtimePath()+" executable=true")
	} else {
		add("failed", "runtime", "runtime binary "+b.runtimePath()+" is not executable on the containerd host")
	}
	if b.cfg.Containerd.Runtime == "krun" {
		if result, err := runner.runShell(ctx, "ldconfig -p 2>/dev/null | grep -m1 libkrun || true", nil, nil); err == nil && strings.Contains(result.Stdout, "libkrun") {
			add("ok", "krun-library", "libkrun is on the loader path")
		} else {
			add("failed", "krun-library", "libkrun is not on the loader path; install libkrun/libkrunfw and a crun built with --with-libkrun")
		}
	}
	if b.cfg.Containerd.Rootless {
		for _, binary := range []string{"newuidmap", "newgidmap"} {
			if b.hostPathExists(ctx, "/usr/bin/"+binary) || b.hostPathExists(ctx, "/usr/sbin/"+binary) {
				add("ok", binary, "present=true")
			} else {
				add("failed", binary, binary+" is not on the containerd host; install the uidmap package and configure subuid/subgid")
			}
		}
		if b.hostPathExists(ctx, "/dev/fuse") {
			add("ok", "fuse", "/dev/fuse present=true")
		} else {
			add("failed", "fuse", "/dev/fuse is missing; rootless overlayfs/fuse-overlayfs cannot mount it")
		}
	}
	if profile := strings.TrimSpace(b.cfg.Containerd.SeccompProfile); profile != "" {
		switch {
		case !b.cfg.Browser && !b.cfg.Desktop:
			// The profile is only applied to browser and desktop leases; reporting
			// its absence as a failure on every plain lease is a false alarm.
			add("skip", "seccomp-profile", profile+" is required only for browser and desktop leases")
		case b.hostPathExists(ctx, profile):
			add("ok", "seccomp-profile", profile+" present=true")
		default:
			add("failed", "seccomp-profile", profile+" is missing; install the narrow profile from CONTAINERD.md §3.1")
		}
	}
	if b.cfg.Containerd.Runtime == "krun" || b.cfg.Containerd.Nested == "on" {
		if b.hostPathExists(ctx, "/dev/kvm") {
			add("ok", "kvm", "/dev/kvm present=true")
		} else {
			add("failed", "kvm", "/dev/kvm is missing; a krun guest or nested container cannot start")
		}
	}
	if b.cfg.Containerd.GPU == "nvidia" {
		if b.hostPathExists(ctx, "/var/run/cdi/nvidia.yaml") || b.hostPathExists(ctx, "/etc/cdi/nvidia.yaml") {
			add("ok", "cdi", "nvidia CDI spec present=true")
		} else if b.cfg.Containerd.Rootless {
			add("failed", "cdi", "rootless CDI cannot see the rootful spec; link it with `ln -sf /var/run/cdi/nvidia.yaml ~/.config/cdi/nvidia.yaml`")
		} else {
			add("failed", "cdi", "no nvidia CDI spec in /var/run/cdi or /etc/cdi; run `nvidia-ctk cdi generate --output=/var/run/cdi/nvidia.yaml`")
		}
	}
	switch b.cfg.Containerd.CRIU {
	case "off":
		add("skip", "criu", "containerd.criu=off")
	case "auto":
		add("skip", "criu", "containerd.criu=auto narrows to off because the automatic pre-flight belongs to the checkpoint seam; set in-container or on to opt in")
	default:
		result, err := runner.runShell(ctx, "criu check --unprivileged --feature seccomp_suspend 2>&1", nil, nil)
		if err != nil || !strings.Contains(result.Stdout, "seccomp_suspend is supported") {
			add("failed", "criu", "criu check --unprivileged --feature seccomp_suspend did not report `seccomp_suspend is supported`; install a CRIU that supports the seccomp feature and run the probe on the containerd host")
		} else {
			add("ok", "criu", "criu check rc=0 and seccomp_suspend is supported")
		}
	}
	if views, err := b.listContainers(ctx); err == nil {
		add("ok", "inventory", "containerd leases="+strconv.Itoa(len(views)))
		for _, view := range views {
			if view.Labels["state"] == "provisioning" {
				add("skip", "pending-lease", "lease="+view.Labels["lease"]+" state=provisioning")
			}
		}
	}
	return core.DoctorResult{Provider: providerName, Checks: checks}, nil
}

func mustJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "[]"
	}
	return string(encoded)
}

// remember/forget keep the backend's own acquisition visible without a second
// inventory read.
func (b *backend) remember(lease core.LeaseTarget) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.acquired[lease.LeaseID] = lease
}

func (b *backend) forget(leaseID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.acquired, leaseID)
}

func (b *backend) remembered(id string) (core.LeaseTarget, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, lease := range b.acquired {
		if id == "" || id == lease.LeaseID || id == lease.Server.Name || id == core.ServerSlug(lease.Server) {
			return lease, true
		}
	}
	return core.LeaseTarget{}, false
}

func shortID(id string) string {
	id = strings.TrimSpace(id)
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// ---- SSH target, port allocation, forwarding, and the readiness hook ----

func (b *backend) sshTargetForPort(ctx context.Context, leaseID, hostPort string) (core.SSHTarget, error) {
	target := core.SSHTargetFromConfig(b.cfg, "127.0.0.1")
	target.TargetOS = core.TargetLinux
	target.Port = hostPort
	// The lease SSH user is the in-container user, never the operator's global
	// SSH user.
	target.User = b.cfg.Containerd.User
	target.Key = ""
	if err := core.UseStoredTestboxKey(&target, leaseID); err != nil {
		return core.SSHTarget{}, err
	}
	// In remote mode startForward replaces Host/Port with the operator-side end
	// of the tunnel; the published port stays on the remote host's loopback.
	return target, nil
}

func (b *backend) startForward(ctx context.Context, leaseID, remotePort string, target *core.SSHTarget) error {
	if !b.runner().remote() {
		return nil
	}
	b.stopForward(leaseID)
	localPort, err := allocateLoopbackPort()
	if err != nil {
		return err
	}
	forwardCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	forward := &sshForward{cancel: cancel, localPort: localPort}
	b.mu.Lock()
	b.forwards[leaseID] = forward
	b.mu.Unlock()
	args := b.runner().remoteForwardArgv(localPort, remotePort)
	go func() {
		_, _ = b.rt.Exec.Run(forwardCtx, core.LocalCommandRequest{
			Name:                    "ssh",
			Args:                    args,
			RequireProcessGroupJoin: true,
		})
	}()
	if err := b.waitForPort(ctx, localPort); err != nil {
		b.stopForward(leaseID)
		return core.Exit(5, "containerd SSH forward did not start on 127.0.0.1:%s: %v", localPort, err)
	}
	target.Port = localPort
	target.Host = "127.0.0.1"
	return nil
}

func (b *backend) stopForward(leaseID string) {
	b.mu.Lock()
	forward := b.forwards[leaseID]
	delete(b.forwards, leaseID)
	b.mu.Unlock()
	if forward != nil && forward.cancel != nil {
		forward.cancel()
	}
}

func (b *backend) allocatePort(ctx context.Context) (string, error) {
	if !b.runner().remote() {
		return allocateLoopbackPort()
	}
	// The published port must be free on the REMOTE host. Ask that host rather
	// than guessing from the operator's namespace.
	result, err := b.runner().runShell(ctx, `python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()'`, nil, nil)
	if err != nil || result.ExitCode != 0 {
		return "", core.Exit(2, "remote port allocation needs python3 on the containerd host (%s); install python3 or run the provider on the host", strings.TrimSpace(result.Stderr))
	}
	port := strings.TrimSpace(result.Stdout)
	if _, err := strconv.Atoi(port); err != nil {
		return "", core.Exit(5, "remote port allocator returned %q, not a port", port)
	}
	return port, nil
}

func allocateLoopbackPort() (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", core.Exit(2, "allocate a free loopback port: %v", err)
	}
	defer listener.Close()
	return strconv.Itoa(listener.Addr().(*net.TCPAddr).Port), nil
}

// waitForPort is a readiness probe, not a retry: it waits for the listener the
// container just published, bounded by a deadline.
func (b *backend) waitForPort(ctx context.Context, port string) error {
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	addr := "127.0.0.1:" + port
	result, err := shared.Poll(waitCtx, 0, 100*time.Millisecond, shared.SleepContext,
		func(ctx context.Context) (bool, error) {
			conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
			if err != nil {
				return false, nil
			}
			_ = conn.Close()
			return true, nil
		},
		func(_ context.Context, ready bool, _ error) (bool, error) { return ready, nil },
		nil,
	)
	if err != nil {
		return shared.PollTerminationError(waitCtx, err, core.Exit(5, "timed out waiting for containerd to publish 127.0.0.1:%s", port))
	}
	if !result.Value {
		return core.Exit(5, "containerd did not publish 127.0.0.1:%s", port)
	}
	return nil
}

// runReadinessHook runs an image-provided readiness probe over SSH and reports
// the elapsed time. An image without the probe is ready by contract.
func (b *backend) runReadinessHook(ctx context.Context, leaseID string, target core.SSHTarget) error {
	started := b.now()
	hookCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	script := `if [ -x /usr/local/libexec/crabbox/ready ]; then /usr/local/libexec/crabbox/ready; fi`
	result, err := shared.Poll(hookCtx, 0, 250*time.Millisecond, shared.SleepContext,
		func(ctx context.Context) (bool, error) {
			run, runErr := b.runOverTarget(ctx, target, script, nil)
			if runErr == nil && run.ExitCode == 0 {
				return true, nil
			}
			return false, nil
		},
		func(_ context.Context, ready bool, _ error) (bool, error) { return ready, nil },
		nil,
	)
	if err != nil || !result.Value {
		return core.Exit(5, "the readiness hook /usr/local/libexec/crabbox/ready did not exit 0 within 5m (image contract: CONTAINERD-IMAGES.md §2); a fork or a k3s node is Ready before it is usable")
	}
	fmt.Fprintf(b.rt.Stderr, "readiness hook completed in %s\n", b.now().Sub(started).Round(time.Millisecond))
	return nil
}

func (b *backend) runOverLeaseSSH(ctx context.Context, lease core.LeaseTarget, script string) error {
	_, err := b.runOverTarget(ctx, lease.SSH, script, nil)
	return err
}

// runOverTarget executes one command over the lease SSH target. It is the only
// path for guest commands; nerdctl exec is never used (krun has no exec).
func (b *backend) runOverTarget(ctx context.Context, target core.SSHTarget, script string, stdin *strings.Reader) (core.LocalCommandResult, error) {
	args := []string{
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "ConnectTimeout=10",
	}
	if target.Port != "" {
		args = append(args, "-p", target.Port)
	}
	if target.Key != "" {
		args = append(args, "-i", target.Key, "-o", "IdentitiesOnly=yes")
	}
	args = append(args, target.User+"@"+target.Host, "sh -c "+core.ShellQuote(script))
	req := core.LocalCommandRequest{Name: "ssh", Args: args, Stdout: nil, Stderr: nil}
	if stdin != nil {
		req.Stdin = stdin
	}
	return b.rt.Exec.Run(ctx, req)
}

func (b *backend) refuseUnsupportedImageProfile(ctx context.Context) error {
	if b.cfg.Containerd.Runtime != "krun" || b.cfg.Containerd.KrunNetwork != "tsi" {
		return nil
	}
	network, err := b.imageLabel(ctx, "crabbox.krun.network")
	if err != nil || network != "passt" {
		return nil
	}
	return core.Exit(2, "the lease image declares crabbox.krun.network=passt, but containerd.krunNetwork=tsi collapses the pod network namespace and breaks CRIU's loopback probe; set containerd.krunNetwork: passt")
}
