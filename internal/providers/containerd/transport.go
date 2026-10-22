package containerd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
	"github.com/openclaw/crabbox/internal/providers/shared/containerbootstrap"
)

const (
	defaultSSHPort    = "2222"
	containerReadyDir = "/usr/local/libexec/crabbox"
	inputsMount       = "/crabbox"
)

// daemonInfo is the verified identity of the containerd daemon the provider
// may act on. Rootless is derived from SecurityOptions, never from Plugins.
type daemonInfo struct {
	ID              string
	Namespace       string
	SecurityOptions []string
}

func (d daemonInfo) rootless() bool {
	for _, option := range d.SecurityOptions {
		if strings.EqualFold(strings.TrimSpace(option), "name=rootless") {
			return true
		}
	}
	return false
}

// commandRunner is the provider's only subprocess boundary. Every local and
// remote command goes through Runtime.Exec so tests can record argv.
type commandRunner struct {
	cfg core.Config
	rt  core.Runtime
}

func (r commandRunner) remote() bool {
	return strings.TrimSpace(r.cfg.Containerd.SSHHost) != ""
}

// run executes nerdctl (local) or nerdctl over SSH (remote).
func (r commandRunner) run(ctx context.Context, args []string, stdout, stderr io.Writer) (core.LocalCommandResult, error) {
	if !r.remote() {
		return r.rt.Exec.Run(ctx, core.LocalCommandRequest{Name: "nerdctl", Args: args, Stdout: stdout, Stderr: stderr})
	}
	return r.rt.Exec.Run(ctx, core.LocalCommandRequest{Name: "ssh", Args: r.remoteArgv(quoteArgv(args)), Stdout: stdout, Stderr: stderr})
}

// runInput executes a local or remote command with caller-supplied stdin. It is
// used to write inputs on the remote host without putting file contents on argv.
func (r commandRunner) runInput(ctx context.Context, args []string, stdin string, stdout, stderr io.Writer) (core.LocalCommandResult, error) {
	req := core.LocalCommandRequest{Name: "nerdctl", Args: args, Stdin: strings.NewReader(stdin), Stdout: stdout, Stderr: stderr}
	if r.remote() {
		req.Name = "ssh"
		req.Args = r.remoteArgv(quoteArgv(args))
	}
	return r.rt.Exec.Run(ctx, req)
}

// runShell executes a POSIX shell command on the containerd host. Remote mode
// keeps the command on the SSH command line (shell-quoted), not on nerdctl.
func (r commandRunner) runShell(ctx context.Context, script string, stdout, stderr io.Writer) (core.LocalCommandResult, error) {
	if r.remote() {
		return r.rt.Exec.Run(ctx, core.LocalCommandRequest{Name: "ssh", Args: r.remoteArgv("sh -c " + core.ShellQuote(script)), Stdout: stdout, Stderr: stderr})
	}
	return r.rt.Exec.Run(ctx, core.LocalCommandRequest{Name: "sh", Args: []string{"-c", script}, Stdout: stdout, Stderr: stderr})
}

// runShellInput runs a POSIX shell command with caller-supplied stdin on the
// containerd host. It exists so remote input files are written with stdin, not
// on argv.
func (r commandRunner) runShellInput(ctx context.Context, script, stdin string) (core.LocalCommandResult, error) {
	req := core.LocalCommandRequest{Name: "sh", Args: []string{"-c", script}, Stdin: strings.NewReader(stdin)}
	if r.remote() {
		req.Name = "ssh"
		req.Args = r.remoteArgv("sh -c " + core.ShellQuote(script))
	}
	return r.rt.Exec.Run(ctx, req)
}

func (r commandRunner) remoteArgv(remote string) []string {
	args := []string{
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "ConnectTimeout=10",
	}
	if port := strings.TrimSpace(r.cfg.Containerd.SSHPort); port != "" && port != "22" {
		args = append(args, "-p", port)
	}
	if key := strings.TrimSpace(r.cfg.Containerd.SSHKey); key != "" {
		args = append(args, "-i", key, "-o", "IdentitiesOnly=yes")
	}
	user := strings.TrimSpace(r.cfg.Containerd.SSHUser)
	if user == "" {
		user = "root"
	}
	return append(args, user+"@"+r.cfg.Containerd.SSHHost, remote)
}

// remoteForwardArgv builds the local-to-remote tunnel that publishes the
// container's SSH port on the operator's loopback.
func (r commandRunner) remoteForwardArgv(localPort, remotePort string) []string {
	args := []string{
		"-N",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "ConnectTimeout=10",
		"-o", "ExitOnForwardFailure=yes",
	}
	if port := strings.TrimSpace(r.cfg.Containerd.SSHPort); port != "" && port != "22" {
		args = append(args, "-p", port)
	}
	if key := strings.TrimSpace(r.cfg.Containerd.SSHKey); key != "" {
		args = append(args, "-i", key, "-o", "IdentitiesOnly=yes")
	}
	user := strings.TrimSpace(r.cfg.Containerd.SSHUser)
	if user == "" {
		user = "root"
	}
	return append(args,
		"-L", "127.0.0.1:"+localPort+":127.0.0.1:"+remotePort,
		user+"@"+r.cfg.Containerd.SSHHost,
	)
}

// globalArgs is the nerdctl selection prefix: namespace, snapshotter, address.
func (r commandRunner) globalArgs() []string {
	c := r.cfg.Containerd
	args := []string{}
	if ns := strings.TrimSpace(c.Namespace); ns != "" {
		args = append(args, "-n", ns)
	}
	if snapshotter := strings.TrimSpace(c.Snapshotter); snapshotter != "" {
		args = append(args, "--snapshotter", snapshotter)
	}
	if address := strings.TrimSpace(c.Address); address != "" {
		args = append(args, "--address", address)
	}
	return args
}

func (r commandRunner) nerdctl(ctx context.Context, args ...string) (core.LocalCommandResult, error) {
	return r.run(ctx, append(r.globalArgs(), args...), nil, nil)
}

func (r commandRunner) nerdctlOutput(ctx context.Context, args ...string) (core.LocalCommandResult, error) {
	var stdout, stderr strings.Builder
	result, err := r.run(ctx, append(r.globalArgs(), args...), &stdout, &stderr)
	// A runner may capture internally (the production runner does) and also tee
	// to the writer; keep whichever produced content.
	if stdout.Len() > 0 {
		result.Stdout = stdout.String()
	}
	if stderr.Len() > 0 {
		result.Stderr = stderr.String()
	}
	return result, err
}

func (b *backend) verifyDaemon(ctx context.Context) (daemonInfo, error) {
	runner := b.runner()
	result, err := runner.nerdctlOutput(ctx, "info", "--format", "{{json .}}")
	if err != nil {
		return daemonInfo{}, shared.LocalCommandError("nerdctl info", result, err)
	}
	var raw struct {
		ID              string   `json:"ID"`
		Namespace       string   `json:"Namespace"`
		SecurityOptions []string `json:"SecurityOptions"`
	}
	if err := json.Unmarshal([]byte(result.Stdout), &raw); err != nil {
		return daemonInfo{}, core.Exit(5, "nerdctl info returned invalid JSON: %v", err)
	}
	if strings.TrimSpace(raw.ID) == "" {
		return daemonInfo{}, core.Exit(5, "nerdctl info did not report a daemon ID; refusing to act on an unidentified daemon")
	}
	info := daemonInfo{ID: raw.ID, Namespace: raw.Namespace, SecurityOptions: raw.SecurityOptions}
	if want := b.cfg.Containerd.Rootless; info.rootless() != want {
		mode := "rootful"
		if info.rootless() {
			mode = "rootless"
		}
		return daemonInfo{}, core.Exit(2, "reached containerd daemon %s is %s but containerd.rootless=%v; a non-root process pointed at a rootful socket silently reroutes to the rootless daemon; set containerd.address to the %s socket (rootless: $XDG_RUNTIME_DIR/containerd-rootless/containerd.sock) or correct containerd.rootless", info.ID, mode, want, mode)
	}
	if err := b.verifyNamespace(ctx, info); err != nil {
		return daemonInfo{}, err
	}
	return info, nil
}

func (b *backend) verifyNamespace(ctx context.Context, info daemonInfo) error {
	namespace := strings.TrimSpace(b.cfg.Containerd.Namespace)
	if namespace == "" {
		return core.Exit(2, "containerd.namespace must not be empty; use the uniform default namespace: default")
	}
	result, err := b.runner().nerdctlOutput(ctx, "namespace", "ls", "--quiet")
	if err != nil {
		return shared.LocalCommandError("nerdctl namespace ls", result, err)
	}
	for _, line := range strings.Split(result.Stdout, "\n") {
		if strings.TrimSpace(line) == namespace {
			return nil
		}
	}
	return core.Exit(2, "containerd namespace %q does not exist on daemon %s; create it with `nerdctl namespace create %s` or set containerd.namespace to an existing namespace", namespace, info.ID, namespace)
}

// buildRunArgs renders `nerdctl run` for one lease. Every feature rides SSH:
// the container publishes sshd on 2222 and never needs nerdctl exec.
func (b *backend) buildRunArgs(ctx context.Context, leaseID, slug, name, inputsDir, stateVolume, hostPort string, extraPorts []string, keep bool, info daemonInfo) ([]string, error) {
	c := b.cfg.Containerd
	args := append(b.runner().globalArgs(), "run", "-d",
		"--name", name,
		"--hostname", name,
		"--network", c.Network,
		"-p", "127.0.0.1:"+hostPort+":"+defaultSSHPort,
		"-v", inputsDir+":"+inputsMount+":ro",
		"-e", "CRABBOX_SSH_USER="+c.User,
		"-e", "CRABBOX_WORK_ROOT="+c.WorkRoot,
		"-e", "CRABBOX_SSH_PORT="+defaultSSHPort,
		"-e", "CRABBOX_NODE_ADDRESS="+c.NodeAddress,
		"-e", "CRABBOX_DESKTOP="+boolString(b.cfg.Desktop),
		"-e", "CRABBOX_DESKTOP_ENV="+core.NormalizedDesktopEnv(b.cfg.DesktopEnv),
		"-e", "CRABBOX_BROWSER="+boolString(b.cfg.Browser),
	)
	for _, mapping := range extraPorts {
		args = append(args, "-p", mapping)
	}
	if stateVolume != "" {
		args = append(args, "-v", stateVolume+":/var")
	}
	for index, volume := range b.cfg.Cache.Volumes {
		if strings.TrimSpace(volume.Path) == "" {
			continue
		}
		// Cache volumes are keyed, not lease-scoped, so a later lease reuses the
		// warm cache. Release never removes them.
		args = append(args, "-v", cacheVolumeName(volume)+":"+volume.Path)
		args = append(args, "-e", fmt.Sprintf("CRABBOX_CACHE_VOLUME_PATH_%d=%s", index, volume.Path))
	}

	krun := c.Runtime == "krun"
	if c.InitMode == "direct" {
		if krun {
			args = append(args, "--stop-signal", "SIGINT")
		}
	} else if krun {
		args = append(args,
			"-e", "KRUN_INIT_PID1=1",
			"--ulimit", "nofile=524288:524288",
		)
	} else {
		args = append(args, "--systemd=always")
	}

	if c.CPUs > 0 {
		args = append(args, "--cpus", strconv.Itoa(c.CPUs))
	}
	if memory := strings.TrimSpace(c.Memory); memory != "" {
		args = append(args, "--memory", memory)
		if krun {
			args = append(args, "--memory-swap", memory)
			ramMiB, err := krunRAMMiB(memory, c.KrunRAMHeadroomMiB)
			if err != nil {
				return nil, err
			}
			args = append(args, "--annotation", "krun.ram_mib="+strconv.Itoa(ramMiB))
		}
	}
	if krun && c.CPUs > 0 {
		args = append(args, "--annotation", "krun.cpus="+strconv.Itoa(c.CPUs))
	}
	if krun && c.KrunNestedVirt {
		args = append(args, "--annotation", "krun.nested_virt=1")
	}
	if krun && c.KrunNetwork == "passt" {
		args = append(args, "--annotation", "krun.use_passt=1")
	}
	if krun && c.KrunKernel == "image" {
		args = append(args, "--annotation", "krun.custom_kernel=1")
	}

	switch c.GPU {
	case "virtio":
		args = append(args, "--annotation", "krun.gpu_flags="+strconv.Itoa(c.KrunGPUFlags))
	case "drm":
		args = append(args, "--device", "/dev/dri", "--group-add", "video", "--group-add", "render")
		if b.hostPathExists(ctx, "/dev/kfd") {
			args = append(args, "--device", "/dev/kfd")
		}
	case "nvidia":
		args = append(args,
			"--device", "nvidia.com/gpu=all",
			"-v", "/usr/share/nvidia/files.d:/usr/share/nvidia/files.d:ro",
		)
	}

	// Browser and desktop leases need a profile that allows Chromium's sandbox;
	// unconfined seccomp is never an acceptable substitute.
	if (b.cfg.Browser || b.cfg.Desktop) && !krun {
		args = append(args, "--security-opt", "seccomp="+c.SeccompProfile)
	}
	// The in-container CRIU mode owns its own capability set; it is mutually
	// exclusive with the nested privileged mode.
	if c.CRIU == "in-container" {
		args = append(args,
			"--security-opt", "seccomp=unconfined",
			"--cap-add", "SYS_ADMIN",
			"--cap-add", "SYS_PTRACE",
			"--cap-add", "CHECKPOINT_RESTORE",
		)
	}
	if !krun && c.Nested == "on" {
		args = append(args,
			"--privileged",
			"--security-opt", "privileged-without-host-devices",
			"--device", "/dev/kmsg",
		)
		if b.hostPathExists(ctx, "/dev/kvm") {
			args = append(args, "--device", "/dev/kvm")
		}
	}
	if b.cfg.Network == core.NetworkTailscale && b.cfg.Tailscale.Enabled {
		args = append(args, "--device", "/dev/net/tun", "--cap-add", "NET_ADMIN")
	}

	for key, value := range b.leaseLabels(leaseID, slug, info, stateVolume, inputsDir, keep) {
		args = append(args, "--label", key+"="+value)
	}

	args = append(args, "--runtime", b.runtimePath())
	args = append(args, c.Image)
	if c.InitMode == "direct" {
		args = append(args, "/bin/sh", inputsMount+"/bootstrap.sh")
	}
	return args, nil
}

// krunRAMMiB converts the lease memory to guest RAM minus the host headroom.
func krunRAMMiB(memory string, headroomMiB int) (int, error) {
	total, err := parseMemoryMiB(memory)
	if err != nil {
		return 0, err
	}
	if total <= headroomMiB {
		return 0, core.Exit(2, "containerd.memory %s leaves no guest RAM after krunRamHeadroomMiB=%d; raise containerd.memory (a krun GPU guest OOM-kills Vulkan compute at 1 GiB)", memory, headroomMiB)
	}
	return total - headroomMiB, nil
}

func parseMemoryMiB(value string) (int, error) {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return 0, core.Exit(2, "containerd.memory must not be empty when a memory limit is required")
	}
	units := []struct {
		suffix string
		factor float64
	}{
		{"gi", 1024}, {"g", 1024},
		{"mi", 1}, {"m", 1},
		{"ki", 1.0 / 1024}, {"k", 1.0 / 1024},
	}
	number := value
	factor := 1.0
	for _, unit := range units {
		if strings.HasSuffix(value, unit.suffix) {
			number = strings.TrimSuffix(value, unit.suffix)
			factor = unit.factor
			break
		}
	}
	parsed, err := strconv.ParseFloat(number, 64)
	if err != nil || parsed <= 0 {
		return 0, core.Exit(2, "invalid containerd.memory %q; use a positive value such as 8g or 4096m", value)
	}
	return int(parsed * factor), nil
}

func (b *backend) runtimePath() string {
	if path := strings.TrimSpace(b.cfg.Containerd.RuntimePath); path != "" {
		return path
	}
	return "/usr/bin/" + b.cfg.Containerd.Runtime
}

func (b *backend) leaseLabels(leaseID, slug string, info daemonInfo, stateVolume, inputsDir string, keep bool) map[string]string {
	c := b.cfg.Containerd
	now := b.now()
	labels := core.DirectLeaseLabels(b.cfg, leaseID, slug, providerName, "", keep, now)
	labels["state"] = "ready"
	labels["namespace"] = c.Namespace
	labels["runtime"] = c.Runtime
	labels["snapshotter"] = c.Snapshotter
	labels["rootless"] = strconv.FormatBool(c.Rootless)
	labels["init"] = c.InitMode
	labels["nested"] = c.Nested
	labels["host"] = b.hostScope()
	labels["daemon"] = info.ID
	labels["image"] = c.Image
	labels["ssh_user"] = c.User
	labels["ssh_port"] = defaultSSHPort
	labels["work_root"] = c.WorkRoot
	labels["state_volume"] = stateVolume
	labels["inputs_dir"] = inputsDir
	if c.GPU != "off" {
		labels["gpu"] = c.GPU
	}
	return labels
}

func (b *backend) hostScope() string {
	if host := strings.TrimSpace(b.cfg.Containerd.SSHHost); host != "" {
		return host
	}
	return "local"
}

// quoteArgv renders an argv for a remote shell without losing argument
// boundaries. nerdctl itself is named on the remote command line.
func cacheVolumeName(volume core.CacheVolumeConfig) string {
	raw := strings.TrimSpace(volume.Key)
	if raw == "" {
		raw = strings.TrimSpace(volume.Name)
	}
	var builder strings.Builder
	for _, r := range strings.ToLower(raw) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			builder.WriteRune(r)
		default:
			builder.WriteByte('-')
		}
	}
	name := strings.Trim(builder.String(), "-")
	if name == "" {
		name = "default"
	}
	return "crabbox-cache-" + name
}

func boolString(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

func quoteArgv(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, core.ShellQuote(arg))
	}
	return strings.Join(quoted, " ")
}

// hostPathExists reports whether a path exists on the containerd host. Local
// and remote checks use the same shell so the answer comes from the host that
// will run the container.
func (b *backend) hostPathExists(ctx context.Context, path string) bool {
	runner := b.runner()
	result, err := runner.runShell(ctx, "test -e "+core.ShellQuote(path), nil, nil)
	return err == nil && result.ExitCode == 0
}

// imageLabel reads one label from the lease image's config so a krun TSI
// mismatch is refused before the container starts.
func (b *backend) imageLabel(ctx context.Context, key string) (string, error) {
	result, err := b.runner().nerdctlOutput(ctx, "image", "inspect", "--format", "{{json .}}", b.cfg.Containerd.Image)
	if err != nil {
		return "", shared.LocalCommandError("nerdctl image inspect", result, err)
	}
	var inspected struct {
		Labels map[string]string `json:"Labels"`
	}
	if err := json.Unmarshal([]byte(result.Stdout), &inspected); err != nil {
		return "", nil
	}
	return inspected.Labels[key], nil
}

func (b *backend) now() time.Time {
	if b.rt.Clock != nil {
		return b.rt.Clock.Now().UTC()
	}
	return time.Now().UTC()
}

func bootstrapScript() string { return containerbootstrap.Script }
