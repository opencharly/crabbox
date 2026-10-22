package cli

import "flag"

//go:generate go run ../../scripts/configgen -source config_containerd.go -output config_containerd_generated.go -type ContainerdConfig -provider containerd

// ContainerdConfig owns the containerd provider's routing and lease settings.
// The address, runtime, snapshotter and namespace identify the daemon the
// provider may act on; the rest describe the container a lease creates.
//
// The SSH key is a PATH. Key material never enters configuration or argv.
//
//configgen:flag-application manual
type ContainerdConfig struct {
	// Daemon identity and connection.
	Address string `config:"address" env:"CRABBOX_CONTAINERD_ADDRESS" flag:"containerd-address" sources:"user,repo,env,flag" help:"containerd socket or nerdctl --address value" default:"/run/containerd/containerd.sock" fileIgnoreEmpty:"true" fileStorage:"value"`
	SSHHost string `config:"sshHost" env:"CRABBOX_CONTAINERD_SSH_HOST" flag:"containerd-ssh-host" sources:"user,repo,env,flag" help:"remote Linux host running containerd; empty selects the local daemon" fileIgnoreEmpty:"true" fileStorage:"value"`
	SSHUser string `config:"sshUser" env:"CRABBOX_CONTAINERD_SSH_USER" flag:"containerd-ssh-user" sources:"user,repo,env,flag" help:"SSH user on the remote containerd host" default:"root" fileIgnoreEmpty:"true" fileStorage:"value"`
	SSHPort string `config:"sshPort" env:"CRABBOX_CONTAINERD_SSH_PORT" flag:"containerd-ssh-port" sources:"user,repo,env,flag" help:"SSH port on the remote containerd host" default:"22" fileIgnoreEmpty:"true" fileStorage:"value"`
	SSHKey  string `config:"sshKey" env:"CRABBOX_CONTAINERD_SSH_KEY" flag:"containerd-ssh-key" sources:"user,repo,env,flag" help:"path to the SSH identity for the remote containerd host; never key material" fileIgnoreEmpty:"true" fileStorage:"value"`

	// Daemon mode and runtime selection.
	Rootless    bool   `config:"rootless" env:"CRABBOX_CONTAINERD_ROOTLESS" flag:"containerd-rootless" sources:"user,repo,env,flag" help:"require the reached daemon to be the user's rootless containerd (verified, fail closed)" default:"false"`
	Runtime     string `config:"runtime" env:"CRABBOX_CONTAINERD_RUNTIME" flag:"containerd-runtime" sources:"user,repo,env,flag" help:"containerd runtime handler: crun, krun, or runc" default:"crun" fileIgnoreEmpty:"true" fileStorage:"value"`
	RuntimePath string `config:"runtimePath" env:"CRABBOX_CONTAINERD_RUNTIME_PATH" flag:"containerd-runtime-path" sources:"user,repo,env,flag" help:"runtime binary passed as --runtime; empty derives /usr/bin/<runtime>" fileIgnoreEmpty:"true" fileStorage:"value"`
	Snapshotter string `config:"snapshotter" env:"CRABBOX_CONTAINERD_SNAPSHOTTER" flag:"containerd-snapshotter" sources:"user,repo,env,flag" help:"snapshotter: overlayfs, native, fuse-overlayfs, or btrfs" default:"overlayfs" fileIgnoreEmpty:"true" fileStorage:"value"`
	Namespace   string `config:"namespace" env:"CRABBOX_CONTAINERD_NAMESPACE" flag:"containerd-namespace" sources:"user,repo,env,flag" help:"containerd namespace; the uniform default is default" default:"default" fileIgnoreEmpty:"true" fileStorage:"value"`

	// Lease container settings.
	Image       string `config:"image" env:"CRABBOX_CONTAINERD_IMAGE" flag:"containerd-image" sources:"user,repo,env,flag" help:"stock OCI image for containerd leases; point it at a prepared runner image for the full feature set" default:"docker.io/library/ubuntu:24.04" fileIgnoreEmpty:"true" fileStorage:"value"`
	User        string `config:"user" env:"CRABBOX_CONTAINERD_USER" flag:"containerd-user" sources:"user,repo,env,flag" help:"SSH user created inside a containerd lease" default:"crabbox" fileIgnoreEmpty:"true" fileStorage:"value"`
	WorkRoot    string `config:"workRoot" env:"CRABBOX_CONTAINERD_WORK_ROOT" flag:"containerd-work-root" sources:"user,repo,env,flag" help:"remote Crabbox work root inside a containerd lease" default:"/work/crabbox" fileIgnoreEmpty:"true" fileStorage:"value"`
	Network     string `config:"network" env:"CRABBOX_CONTAINERD_NETWORK" flag:"containerd-network" sources:"user,repo,env,flag" help:"container network for containerd leases" default:"bridge" fileIgnoreEmpty:"true" fileStorage:"value"`
	InitMode    string `config:"initMode" env:"CRABBOX_CONTAINERD_INIT_MODE" flag:"containerd-init-mode" sources:"user,repo,env,flag" help:"init inside the lease: direct (bootstrap runs sshd) or systemd" default:"direct" fileIgnoreEmpty:"true" fileStorage:"value"`
	Nested      string `config:"nested" env:"CRABBOX_CONTAINERD_NESTED" flag:"containerd-nested" sources:"user,repo,env,flag" help:"nested container support: off or on" default:"off" fileIgnoreEmpty:"true" fileStorage:"value"`
	StateVolume string `config:"stateVolume" env:"CRABBOX_CONTAINERD_STATE_VOLUME" flag:"containerd-state-volume" sources:"user,repo,env,flag" help:"attach a per-lease named state volume: auto or off" default:"auto" fileIgnoreEmpty:"true" fileStorage:"value"`
	GPU         string `config:"gpu" env:"CRABBOX_CONTAINERD_GPU" flag:"containerd-gpu" sources:"user,repo,env,flag" help:"GPU mode: off, drm, nvidia, or virtio" default:"off" fileIgnoreEmpty:"true" fileStorage:"value"`
	CRIU        string `config:"criu" env:"CRABBOX_CONTAINERD_CRIU" flag:"containerd-criu" sources:"user,repo,env,flag" help:"CRIU checkpoint mode: off, on, auto, or in-container" default:"auto" fileIgnoreEmpty:"true" fileStorage:"value"`
	CPUs        int    `config:"cpus" env:"CRABBOX_CONTAINERD_CPUS" flag:"containerd-cpus" sources:"user,repo,env,flag" help:"CPU limit for a containerd lease; 0 leaves the runtime default" nonnegative:"true" fileInt:"positive" fileStorage:"value" envInt:"fallback"`
	Memory      string `config:"memory" env:"CRABBOX_CONTAINERD_MEMORY" flag:"containerd-memory" sources:"user,repo,env,flag" help:"memory limit for a containerd lease, for example 8g" fileIgnoreEmpty:"true" fileStorage:"value"`

	// Security profile and krun handler annotations.
	SeccompProfile     string `config:"seccompProfile" env:"CRABBOX_CONTAINERD_SECCOMP_PROFILE" flag:"containerd-seccomp-profile" sources:"user,repo,env,flag" help:"narrow seccomp profile for browser and desktop leases" default:"/etc/containerd/seccomp/crabbox-userns.json" fileIgnoreEmpty:"true" fileStorage:"value"`
	KrunGPUFlags       int    `config:"krunGpuFlags" env:"CRABBOX_CONTAINERD_KRUN_GPU_FLAGS" flag:"containerd-krun-gpu-flags" sources:"user,repo,env,flag" help:"krun.gpu_flags annotation for a virtio GPU (962 default; 1410 is GL-only; 1283 must never be used)" default:"962" nonnegative:"true" fileInt:"positive" fileStorage:"value" envInt:"fallback"`
	KrunRAMHeadroomMiB int    `config:"krunRamHeadroomMiB" env:"CRABBOX_CONTAINERD_KRUN_RAM_HEADROOM_MIB" flag:"containerd-krun-ram-headroom-mib" sources:"user,repo,env,flag" help:"guest RAM reserved for the krun host, subtracted from the lease memory" default:"256" nonnegative:"true" fileInt:"positive" fileStorage:"value" envInt:"fallback"`
	KrunNestedVirt     bool   `config:"krunNestedVirt" env:"CRABBOX_CONTAINERD_KRUN_NESTED_VIRT" flag:"containerd-krun-nested-virt" sources:"user,repo,env,flag" help:"set krun.nested_virt=1 for a krun lease" default:"false"`
	KrunNetwork        string `config:"krunNetwork" env:"CRABBOX_CONTAINERD_KRUN_NETWORK" flag:"containerd-krun-network" sources:"user,repo,env,flag" help:"krun network backend: tsi or passt" default:"tsi" fileIgnoreEmpty:"true" fileStorage:"value"`
	KrunKernel         string `config:"krunKernel" env:"CRABBOX_CONTAINERD_KRUN_KERNEL" flag:"containerd-krun-kernel" sources:"user,repo,env,flag" help:"krun kernel: stock or image" default:"stock" fileIgnoreEmpty:"true" fileStorage:"value"`

	// Node address for the krun guest / inner networking.
	NodeAddress string `config:"nodeAddress" env:"CRABBOX_CONTAINERD_NODE_ADDRESS" flag:"containerd-node-address" sources:"user,repo,env,flag" help:"node address exposed to a krun guest (CIDR)" default:"172.31.255.1/32" fileIgnoreEmpty:"true" fileStorage:"value"`

	// Host volumes remain an explicit CLI creation action, never file or env input.
	Volumes []string `flag:"containerd-volume" sources:"flag" help:"bind-mount a host path or named volume into the container; source:destination[:options]; repeatable" flagList:"append-raw"`
}

func applyContainerdFileConfig(cfg *Config, file *fileContainerdConfig, source configInputSource) error {
	applied, err := cfg.Containerd.applyFile(file)
	recordConfigInput(cfg, "containerd", source, applied.InputAccepted)
	return err
}

func applyContainerdEnvironmentConfig(cfg *Config) error {
	applied, err := cfg.Containerd.applyEnv()
	recordConfigInput(cfg, "containerd", configInputEnvironment, applied.InputAccepted)
	return err
}

// ApplyContainerdConfigFlags copies explicit flag values. Enums and capability
// refusals are validated by the provider in Configure.
func ApplyContainerdConfigFlags(cfg *ContainerdConfig, values ContainerdConfigFlagValues, fs *flag.FlagSet) (ContainerdConfigApplied, error) {
	visited := ContainerdConfigFlagPresence(fs)
	var applied ContainerdConfigApplied
	set := func(seen bool) {
		if seen {
			applied.InputAccepted = true
		}
	}
	if visited.Address {
		cfg.Address = *values.Address
		set(true)
	}
	if visited.SSHHost {
		cfg.SSHHost = *values.SSHHost
		set(true)
	}
	if visited.SSHUser {
		cfg.SSHUser = *values.SSHUser
		set(true)
	}
	if visited.SSHPort {
		cfg.SSHPort = *values.SSHPort
		set(true)
	}
	if visited.SSHKey {
		cfg.SSHKey = *values.SSHKey
		set(true)
	}
	if visited.Rootless {
		cfg.Rootless = *values.Rootless
		set(true)
	}
	if visited.Runtime {
		cfg.Runtime = *values.Runtime
		set(true)
	}
	if visited.RuntimePath {
		cfg.RuntimePath = *values.RuntimePath
		set(true)
	}
	if visited.Snapshotter {
		cfg.Snapshotter = *values.Snapshotter
		set(true)
	}
	if visited.Namespace {
		cfg.Namespace = *values.Namespace
		set(true)
	}
	if visited.Image {
		cfg.Image = *values.Image
		set(true)
	}
	if visited.User {
		cfg.User = *values.User
		set(true)
	}
	if visited.WorkRoot {
		cfg.WorkRoot = *values.WorkRoot
		set(true)
	}
	if visited.Network {
		cfg.Network = *values.Network
		set(true)
	}
	if visited.InitMode {
		cfg.InitMode = *values.InitMode
		set(true)
	}
	if visited.Nested {
		cfg.Nested = *values.Nested
		set(true)
	}
	if visited.StateVolume {
		cfg.StateVolume = *values.StateVolume
		set(true)
	}
	if visited.GPU {
		cfg.GPU = *values.GPU
		set(true)
	}
	if visited.CRIU {
		cfg.CRIU = *values.CRIU
		set(true)
	}
	if visited.CPUs {
		cfg.CPUs = *values.CPUs
		set(true)
	}
	if visited.Memory {
		cfg.Memory = *values.Memory
		set(true)
	}
	if visited.SeccompProfile {
		cfg.SeccompProfile = *values.SeccompProfile
		set(true)
	}
	if visited.KrunGPUFlags {
		cfg.KrunGPUFlags = *values.KrunGPUFlags
		set(true)
	}
	if visited.KrunRAMHeadroomMiB {
		cfg.KrunRAMHeadroomMiB = *values.KrunRAMHeadroomMiB
		set(true)
	}
	if visited.KrunNestedVirt {
		cfg.KrunNestedVirt = *values.KrunNestedVirt
		set(true)
	}
	if visited.KrunNetwork {
		cfg.KrunNetwork = *values.KrunNetwork
		set(true)
	}
	if visited.KrunKernel {
		cfg.KrunKernel = *values.KrunKernel
		set(true)
	}
	if visited.NodeAddress {
		cfg.NodeAddress = *values.NodeAddress
		set(true)
	}
	if visited.Volumes {
		cfg.Volumes = append([]string(nil), (*values.Volumes)...)
		set(true)
	}
	return applied, nil
}
