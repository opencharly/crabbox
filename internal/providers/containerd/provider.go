package containerd

import (
	"flag"
	"strconv"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

const providerName = "containerd"

func init() {
	core.RegisterProvider(Provider{})
}

type Provider struct{}

// BackendCapabilities returns metadata-only capabilities: no configuration,
// credentials, clients, or lifecycle methods are touched.
func (Provider) BackendCapabilities() core.Backend { return &backend{spec: Provider{}.Spec()} }

// Spec is stable, configuration-free metadata. The checkpoint, fork and
// restore features are declared here because the checkpoint seam implements
// their full lifecycle (containerd-commit).
func (Provider) Spec() core.ProviderSpec {
	return core.ProviderSpec{
		Authentication: core.ProviderAuthentication{ // fresh slices on every call
			{
				Route:   "direct",
				Methods: []core.ProviderAuthenticationMethod{core.ProviderAuthenticationNativeConfig},
				Description: "containerd socket access as the invoking user; rootful needs the operator's sudo rule or socket access, " +
					"rootless uses the user's socket.",
			},
			{
				Route:       "remote-host",
				Methods:     []core.ProviderAuthenticationMethod{core.ProviderAuthenticationSSH},
				Description: "SSH access to a remote Linux host that runs containerd; the key path is configuration, never key material.",
			},
		},
		Name:    providerName,
		Kind:    core.ProviderKindSSHLease,
		Targets: []core.TargetSpec{{OS: core.TargetLinux}},
		Features: core.FeatureSet{
			core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup, core.FeatureRunSession,
			core.FeatureCacheVolume,
			core.FeatureCheckpoint, core.FeatureFork, core.FeatureRestore,
			core.FeatureDesktop, core.FeatureBrowser, core.FeatureCode, core.FeatureTailscale,
			core.FeaturePauseResume, core.FeatureClaimExec, core.FeatureFixedCurrentRepoStop,
		},
		Coordinator:              core.CoordinatorNever,
		ClassDisposition:         core.ProviderClassDispositionUnmapped,
		ActionsRunnerUnsupported: false,
	}
}

func (Provider) RegisterFlags(fs *flag.FlagSet, defaults core.Config) any {
	return core.RegisterContainerdConfigFlags(fs, defaults.Containerd)
}

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(core.ContainerdConfigFlagValues)
	if !ok {
		return nil
	}
	applied, err := core.ApplyContainerdConfigFlags(&cfg.Containerd, v, fs)
	core.RecordProviderFlagInputs(cfg, applied.InputAccepted, providerName)
	return err
}

// CreationOnlyFlagNames marks the host-volume list as a creation-time action.
func (Provider) CreationOnlyFlagNames() []string { return []string{"containerd-volume"} }

// ClaimScope is the opaque routing identity core persists on a local claim.
// An incomplete scope is empty so core refuses to create an unscoped lease.
func (Provider) ClaimScope(cfg core.Config) string {
	c := cfg.Containerd
	namespace := strings.TrimSpace(c.Namespace)
	runtime := strings.TrimSpace(c.Runtime)
	snapshotter := strings.TrimSpace(c.Snapshotter)
	if namespace == "" || runtime == "" || snapshotter == "" {
		return ""
	}
	parts := []string{namespace, runtime, snapshotter}
	if c.Rootless {
		parts = append(parts, "rootless")
	}
	initMode := strings.TrimSpace(c.InitMode)
	nested := strings.TrimSpace(c.Nested)
	if initMode == "" || nested == "" {
		return ""
	}
	parts = append(parts, initMode, nested, hostScopeForConfig(cfg))
	return strings.Join(parts, "/")
}

func hostScopeForConfig(cfg core.Config) string {
	if host := strings.TrimSpace(cfg.Containerd.SSHHost); host != "" {
		return host
	}
	return "local"
}

// ConfigShowSection is passive: it derives effective display values from the
// supplied Config and never reads the environment or the native runtime.
func (Provider) ConfigShowSection(cfg core.Config) core.ProviderConfigShowSection {
	c := cfg.Containerd
	runtimePath := c.RuntimePath
	if strings.TrimSpace(runtimePath) == "" {
		runtimePath = "/usr/bin/" + c.Runtime
	}
	return core.ProviderConfigShowSection{
		JSONKey: "containerd", TextLabel: "containerd", Providers: []string{providerName},
		Fields: []core.ProviderConfigShowField{
			{JSONName: "address", JSONValue: c.Address, TextName: "address", TextValue: c.Address},
			{JSONName: "sshHost", JSONValue: c.SSHHost, TextName: "ssh_host", TextValue: core.Blank(c.SSHHost, "-")},
			{JSONName: "sshUser", JSONValue: c.SSHUser, TextName: "ssh_user", TextValue: c.SSHUser},
			{JSONName: "sshPort", JSONValue: c.SSHPort, TextName: "ssh_port", TextValue: c.SSHPort},
			{JSONName: "sshKey", JSONValue: core.ConfigShowSecretState(c.SSHKey), TextName: "ssh_key", TextValue: core.ConfigShowSecretState(c.SSHKey)},
			{JSONName: "rootless", JSONValue: c.Rootless, TextName: "rootless", TextValue: strconv.FormatBool(c.Rootless)},
			{JSONName: "runtime", JSONValue: c.Runtime, TextName: "runtime", TextValue: c.Runtime},
			{JSONName: "runtimePath", JSONValue: runtimePath, TextName: "runtime_path", TextValue: runtimePath},
			{JSONName: "snapshotter", JSONValue: c.Snapshotter, TextName: "snapshotter", TextValue: c.Snapshotter},
			{JSONName: "namespace", JSONValue: c.Namespace, TextName: "namespace", TextValue: c.Namespace},
			{JSONName: "image", JSONValue: c.Image, TextName: "image", TextValue: c.Image},
			{JSONName: "user", JSONValue: c.User, TextName: "user", TextValue: c.User},
			{JSONName: "workRoot", JSONValue: c.WorkRoot, TextName: "work_root", TextValue: c.WorkRoot},
			{JSONName: "network", JSONValue: c.Network, TextName: "network", TextValue: c.Network},
			{JSONName: "initMode", JSONValue: c.InitMode, TextName: "init_mode", TextValue: c.InitMode},
			{JSONName: "nested", JSONValue: c.Nested, TextName: "nested", TextValue: c.Nested},
			{JSONName: "stateVolume", JSONValue: c.StateVolume, TextName: "state_volume", TextValue: c.StateVolume},
			{JSONName: "gpu", JSONValue: c.GPU, TextName: "gpu", TextValue: c.GPU},
			{JSONName: "criu", JSONValue: c.CRIU, TextName: "criu", TextValue: c.CRIU},
			{JSONName: "cpus", JSONValue: c.CPUs, TextName: "cpus", TextValue: strconv.Itoa(c.CPUs)},
			{JSONName: "memory", JSONValue: c.Memory, TextName: "memory", TextValue: core.Blank(c.Memory, "-")},
			{JSONName: "seccompProfile", JSONValue: c.SeccompProfile, TextName: "seccomp_profile", TextValue: core.Blank(c.SeccompProfile, "-")},
			{JSONName: "krunGpuFlags", JSONValue: c.KrunGPUFlags, TextName: "krun_gpu_flags", TextValue: strconv.Itoa(c.KrunGPUFlags)},
			{JSONName: "krunRamHeadroomMiB", JSONValue: c.KrunRAMHeadroomMiB, TextName: "krun_ram_headroom_mib", TextValue: strconv.Itoa(c.KrunRAMHeadroomMiB)},
			{JSONName: "krunNestedVirt", JSONValue: c.KrunNestedVirt, TextName: "krun_nested_virt", TextValue: strconv.FormatBool(c.KrunNestedVirt)},
			{JSONName: "krunNetwork", JSONValue: c.KrunNetwork, TextName: "krun_network", TextValue: c.KrunNetwork},
			{JSONName: "krunKernel", JSONValue: c.KrunKernel, TextName: "krun_kernel", TextValue: c.KrunKernel},
			{JSONName: "nodeAddress", JSONValue: c.NodeAddress, TextName: "node_address", TextValue: c.NodeAddress},
		},
	}
}

// preparedImageFeatures names the prepared runner image that carries each
// optional capability. The base SSH contract runs on any Linux image, but
// desktop, browser, code and tailscale live in the prepared image's layers
// (CONTAINERD-IMAGES.md), so they are admitted only when that image is
// configured.
var preparedImageFeatures = map[core.Feature]string{
	core.FeatureDesktop:   "pod-crabbox-desktop",
	core.FeatureBrowser:   "pod-crabbox-browser",
	core.FeatureCode:      "pod-crabbox-code",
	core.FeatureTailscale: "pod-crabbox-tailscale",
}

// ConfiguredFeatures narrows the static catalog to what the configured image
// can actually serve. It reads only configuration: it never probes the daemon
// or the image, so it is usable before a lease exists.
func (Provider) ConfiguredFeatures(cfg core.Config) core.FeatureSet {
	spec := Provider{}.Spec()
	features := make(core.FeatureSet, 0, len(spec.Features))
	for _, feature := range spec.Features {
		image, gated := preparedImageFeatures[feature]
		if gated && !configuredImageServes(cfg.Containerd.Image, image) {
			continue
		}
		features = append(features, feature)
	}
	return features
}

// configuredImageServes reports whether the configured OCI reference names the
// prepared image. The reference may carry a registry, a port, a tag and a
// digest; only the final path segment is compared.
func configuredImageServes(reference, prepared string) bool {
	ref := reference
	if at := strings.Index(ref, "@"); at >= 0 {
		ref = ref[:at]
	}
	if slash, colon := strings.LastIndex(ref, "/"), strings.LastIndex(ref, ":"); colon > slash {
		ref = ref[:colon]
	}
	if slash := strings.LastIndex(ref, "/"); slash >= 0 {
		ref = ref[slash+1:]
	}
	return ref == prepared
}

// validatePreparedImages refuses a requested feature the configured image
// cannot serve, before any daemon call, and names the image that can.
func validatePreparedImages(cfg core.Config) error {
	requested := []struct {
		flag    string
		on      bool
		feature core.Feature
	}{
		{"--desktop", cfg.Desktop, core.FeatureDesktop},
		{"--browser", cfg.Browser, core.FeatureBrowser},
		{"--code", cfg.Code, core.FeatureCode},
		{"--tailscale", cfg.Tailscale.Enabled, core.FeatureTailscale},
	}
	configured := Provider{}.ConfiguredFeatures(cfg)
	for _, req := range requested {
		if !req.on || configured.Has(req.feature) {
			continue
		}
		return core.Exit(2, "provider=%s cannot serve %s with containerd.image %q; set containerd.image to the prepared image ghcr.io/opencharly/%s, which carries that capability", providerName, req.flag, cfg.Containerd.Image, preparedImageFeatures[req.feature])
	}
	return nil
}

// ValidateRequestedCapabilities refuses only measured real limits, and each
// refusal names the mechanism and a configuration that works.
func (Provider) ValidateRequestedCapabilities(cfg core.Config) error {
	return validateRequestedCapabilities(cfg)
}

func (Provider) ValidateConfig(cfg core.Config) error {
	if cfg.TargetOS != "" && cfg.TargetOS != core.TargetLinux {
		return core.Exit(2, "provider=%s supports target=linux only", providerName)
	}
	return validateConfigEnums(cfg)
}

func (Provider) Configure(cfg core.Config, rt core.Runtime) (core.Backend, error) {
	if cfg.TargetOS != "" && cfg.TargetOS != core.TargetLinux {
		return nil, core.Exit(2, "provider=%s supports target=linux only", providerName)
	}
	if err := validateConfigEnums(cfg); err != nil {
		return nil, err
	}
	if err := validateRequestedCapabilities(cfg); err != nil {
		return nil, err
	}
	return newBackend(Provider{}.Spec(), cfg, rt), nil
}

func validateConfigEnums(cfg core.Config) error {
	c := cfg.Containerd
	enums := []struct {
		key    string
		value  string
		values []string
	}{
		{"containerd.runtime", c.Runtime, []string{"crun", "krun", "runc"}},
		{"containerd.snapshotter", c.Snapshotter, []string{"overlayfs", "native", "fuse-overlayfs", "btrfs"}},
		{"containerd.initMode", c.InitMode, []string{"direct", "systemd"}},
		{"containerd.nested", c.Nested, []string{"off", "on"}},
		{"containerd.stateVolume", c.StateVolume, []string{"auto", "off"}},
		{"containerd.gpu", c.GPU, []string{"off", "drm", "nvidia", "virtio"}},
		{"containerd.criu", c.CRIU, []string{"off", "on", "auto", "in-container"}},
		{"containerd.krunNetwork", c.KrunNetwork, []string{"tsi", "passt"}},
		{"containerd.krunKernel", c.KrunKernel, []string{"stock", "image"}},
	}
	for _, enum := range enums {
		if !contains(enum.values, enum.value) {
			return core.Exit(2, "%s=%q is not supported; use one of %s", enum.key, enum.value, strings.Join(enum.values, ", "))
		}
	}
	if err := validateCRIU(c); err != nil {
		return err
	}
	if strings.TrimSpace(c.Namespace) == "" {
		return core.Exit(2, "containerd.namespace must not be empty; the uniform default is default")
	}
	if strings.TrimSpace(c.Image) == "" {
		return core.Exit(2, "containerd.image must not be empty; set a stock image or a prepared runner image")
	}
	return nil
}

func validateRequestedCapabilities(cfg core.Config) error {
	c := cfg.Containerd
	if err := validateCRIU(c); err != nil {
		return err
	}
	if err := validatePreparedImages(cfg); err != nil {
		return err
	}
	krun := c.Runtime == "krun"
	if (cfg.Actions.Workflow != "" || len(cfg.Actions.RunnerLabels) > 0) && c.InitMode == "direct" {
		return core.Exit(2, "an Actions runner installs a systemd unit inside the lease, but containerd.initMode=direct runs the bootstrap script as PID 1; set containerd.initMode: systemd (the image init owns the unit)")
	}
	if c.Snapshotter == "btrfs" && c.Rootless {
		return core.Exit(2, "a rootless user namespace cannot mount the btrfs device that snapshotter btrfs requires (containerd btrfs.go:240-248, NEEDS UPSTREAM); use containerd.snapshotter: native on a btrfs data root, or overlayfs")
	}
	if krun && c.GPU == "nvidia" {
		return core.Exit(2, "the krun runtime's render-server sandbox dev-binds only /dev/dri (crun handlers/krun.c:699-703, still on main) and cannot inject NVIDIA devices; use containerd.runtime: crun or runc with containerd.gpu: nvidia (CDI), or containerd.gpu: virtio on AMD/Intel")
	}
	if krun && c.GPU == "drm" {
		return core.Exit(2, "krun takes GPU devices through krun.gpu_flags, never --device /dev/dri; use containerd.gpu: virtio for a krun guest, or containerd.runtime: crun or runc with containerd.gpu: drm")
	}
	if (cfg.Desktop || cfg.Browser) && !krun && strings.TrimSpace(c.SeccompProfile) == "" {
		return core.Exit(2, "a browser or desktop lease needs a seccomp profile that allows Chromium's sandbox; set containerd.seccompProfile to the narrow profile from CONTAINERD.md §3.1 (never seccomp=unconfined and never --no-sandbox)")
	}
	if !krun && c.GPU == "virtio" {
		return core.Exit(2, "containerd.gpu: virtio is a krun guest GPU (krun.gpu_flags); with runtime %s use containerd.gpu: drm or nvidia", c.Runtime)
	}
	if krun && c.GPU != "off" && strings.TrimSpace(c.Memory) != "" {
		ramMiB, err := parseMemoryMiB(c.Memory)
		if err != nil {
			return err
		}
		// The guest sees memory minus krunRamHeadroomMiB, so the 4 GiB floor
		// applies to the GUEST RAM, not to the total the runtime is given.
		guestMiB := ramMiB - c.KrunRAMHeadroomMiB
		if guestMiB < 4096 {
			return core.Exit(2, "a krun GPU guest needs at least 4 GiB of guest RAM but containerd.memory %s leaves %d MiB after krunRamHeadroomMiB=%d; raise containerd.memory to at least %dm (4g plus the headroom)", c.Memory, guestMiB, c.KrunRAMHeadroomMiB, 4096+c.KrunRAMHeadroomMiB)
		}
	}
	return nil
}

// validateCRIU fails closed on the modes this build does not implement.
// Process checkpoints are seam 3 (opencharly/crabbox#23); until that lands,
// only off and auto (which already narrows to off) are accepted.
func validateCRIU(c core.ContainerdConfig) error {
	switch c.CRIU {
	case "off", "auto":
		return nil
	default:
		return core.Exit(2, "containerd.criu=%q is not available on this build: process checkpoints are tracked in opencharly/crabbox#23; use containerd.criu: off (auto also narrows to off)", c.CRIU)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
