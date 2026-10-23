package containerd

import (
	"context"
	"errors"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
	_ "github.com/openclaw/crabbox/internal/providers/localcontainer"
)

type recordingRunner struct {
	calls []core.LocalCommandRequest
	run   func(core.LocalCommandRequest) (core.LocalCommandResult, error)
}

func (r *recordingRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	r.calls = append(r.calls, req)
	if r.run != nil {
		return r.run(req)
	}
	return core.LocalCommandResult{}, nil
}

func testBackend(runner *recordingRunner) *backend {
	cfg := core.Config{
		Provider: providerName,
		TargetOS: core.TargetLinux,
		Containerd: core.ContainerdConfig{
			Address:            "/run/containerd/containerd.sock",
			SSHUser:            "root",
			SSHPort:            "22",
			Runtime:            "crun",
			Snapshotter:        "overlayfs",
			Namespace:          "default",
			Image:              "docker.io/library/ubuntu:24.04",
			User:               "crabbox",
			WorkRoot:           "/work/crabbox",
			Network:            "bridge",
			InitMode:           "direct",
			Nested:             "off",
			StateVolume:        "auto",
			GPU:                "off",
			CRIU:               "auto",
			SeccompProfile:     "/etc/containerd/seccomp/crabbox-userns.json",
			KrunGPUFlags:       962,
			KrunRAMHeadroomMiB: 256,
			KrunNetwork:        "tsi",
			KrunKernel:         "stock",
			NodeAddress:        "172.31.255.1/32",
		},
	}
	return &backend{
		spec:     Provider{}.Spec(),
		cfg:      cfg,
		rt:       core.Runtime{Exec: runner, Stderr: &strings.Builder{}},
		forwards: map[string]*sshForward{},
		acquired: map[string]core.LeaseTarget{},
	}
}

func findArg(args []string, want ...string) bool {
	for i := 0; i+len(want) <= len(args); i++ {
		matched := true
		for j, value := range want {
			if args[i+j] != value {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func TestBuildRunArgs(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mutate    func(*core.Config)
		want      [][]string
		forbidden [][]string
	}{
		{
			name: "direct crun",
			want: [][]string{
				{"run", "-d"},
				{"-e", "CRABBOX_SSH_USER=crabbox"},
				{"-e", "CRABBOX_WORK_ROOT=/work/crabbox"},
				{"-e", "CRABBOX_SSH_PORT=2222"},
				{"--name", "crabbox-blue-lobster-00000000"},
				{"--network", "bridge"},
				{"-p", "127.0.0.1:49152:2222"},
				{"-v", "/tmp/inputs:/crabbox:ro"},
				{"-v", "crabbox-cbx_lease-state:/var"},
				{"--runtime", "/usr/bin/crun"},
				{"docker.io/library/ubuntu:24.04", "/bin/sh", "/crabbox/bootstrap.sh"},
			},
			forbidden: [][]string{{"exec"}, {"--pid", "host"}, {"--systemd=always"}},
		},
		{
			name:   "systemd crun",
			mutate: func(c *core.Config) { c.Containerd.InitMode = "systemd" },
			want:   [][]string{{"--systemd=always"}, {"--runtime", "/usr/bin/crun"}},
			forbidden: [][]string{
				{"/bin/sh", "/crabbox/bootstrap.sh"},
			},
		},
		{
			name: "systemd krun",
			mutate: func(c *core.Config) {
				c.Containerd.Runtime = "krun"
				c.Containerd.InitMode = "systemd"
			},
			want:      [][]string{{"-e", "KRUN_INIT_PID1=1"}, {"--ulimit", "nofile=524288:524288"}, {"--runtime", "/usr/bin/krun"}},
			forbidden: [][]string{{"--systemd=always"}},
		},
		{
			name: "direct krun",
			mutate: func(c *core.Config) {
				c.Containerd.Runtime = "krun"
			},
			want: [][]string{{"--stop-signal", "SIGINT"}, {"/bin/sh", "/crabbox/bootstrap.sh"}},
		},
		{
			name:   "cpus and memory",
			mutate: func(c *core.Config) { c.Containerd.CPUs = 4; c.Containerd.Memory = "8g" },
			want:   [][]string{{"--cpus", "4"}, {"--memory", "8g"}},
		},
		{
			name: "krun memory and cpus annotations",
			mutate: func(c *core.Config) {
				c.Containerd.Runtime = "krun"
				c.Containerd.CPUs = 4
				c.Containerd.Memory = "8g"
			},
			want: [][]string{
				{"--memory-swap", "8g"},
				{"--annotation", "krun.ram_mib=7936"},
				{"--annotation", "krun.cpus=4"},
			},
		},
		{
			name: "krun handler annotations",
			mutate: func(c *core.Config) {
				c.Containerd.Runtime = "krun"
				c.Containerd.KrunNestedVirt = true
				c.Containerd.KrunNetwork = "passt"
				c.Containerd.KrunKernel = "image"
			},
			want: [][]string{
				{"--annotation", "krun.nested_virt=1"},
				{"--annotation", "krun.use_passt=1"},
				{"--annotation", "krun.custom_kernel=1"},
			},
			forbidden: [][]string{{"--pid", "host"}},
		},
		{
			name:   "gpu drm",
			mutate: func(c *core.Config) { c.Containerd.GPU = "drm" },
			want:   [][]string{{"--device", "/dev/dri"}, {"--group-add", "video"}, {"--group-add", "render"}},
		},
		{
			name:   "gpu nvidia",
			mutate: func(c *core.Config) { c.Containerd.GPU = "nvidia" },
			want: [][]string{
				{"--device", "nvidia.com/gpu=all"},
				{"-v", "/usr/share/nvidia/files.d:/usr/share/nvidia/files.d:ro"},
			},
		},
		{
			name: "gpu virtio krun",
			mutate: func(c *core.Config) {
				c.Containerd.Runtime = "krun"
				c.Containerd.GPU = "virtio"
			},
			want:      [][]string{{"--annotation", "krun.gpu_flags=962"}},
			forbidden: [][]string{{"--device", "/dev/dri"}},
		},
		{
			name:   "nested privileged",
			mutate: func(c *core.Config) { c.Containerd.Nested = "on" },
			want: [][]string{
				{"--privileged"},
				{"--security-opt", "privileged-without-host-devices"},
				{"--device", "/dev/kmsg"},
			},
		},
		{
			name:   "browser seccomp profile",
			mutate: func(c *core.Config) { c.Browser = true },
			want: [][]string{
				{"--security-opt", "seccomp=/etc/containerd/seccomp/crabbox-userns.json"},
				{"-e", "CRABBOX_BROWSER=1"},
			},
			forbidden: [][]string{
				{"--security-opt", "seccomp=unconfined"},
				{"--no-sandbox"},
			},
		},
		{
			name: "criu in-container",
			mutate: func(c *core.Config) {
				c.Containerd.CRIU = "in-container"
			},
			want: [][]string{
				{"--security-opt", "seccomp=unconfined"},
				{"--cap-add", "SYS_ADMIN"},
				{"--cap-add", "SYS_PTRACE"},
				{"--cap-add", "CHECKPOINT_RESTORE"},
			},
		},
		{
			name: "tailscale kernel mode",
			mutate: func(c *core.Config) {
				c.Network = core.NetworkTailscale
				c.Tailscale.Enabled = true
			},
			want: [][]string{{"--device", "/dev/net/tun"}, {"--cap-add", "NET_ADMIN"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := testBackend(&recordingRunner{})
			if tc.mutate != nil {
				tc.mutate(&b.cfg)
			}
			info := daemonInfo{ID: "daemon-id", SecurityOptions: []string{"name=seccomp"}}
			args, err := b.buildRunArgs(t.Context(), "cbx_lease", "blue-lobster", "crabbox-blue-lobster-00000000", "/tmp/inputs", "crabbox-cbx_lease-state", "49152", nil, false, info)
			if err != nil {
				t.Fatalf("buildRunArgs: %v", err)
			}
			for _, want := range tc.want {
				if !findArg(args, want...) {
					t.Fatalf("missing %v in %v", want, args)
				}
			}
			for _, forbidden := range tc.forbidden {
				if findArg(args, forbidden...) {
					t.Fatalf("forbidden %v present in %v", forbidden, args)
				}
			}
			if !findArg(args, "--runtime") {
				t.Fatalf("--runtime is always required: %v", args)
			}
			if !findArg(args, "--label", "provider=containerd") || !findArg(args, "--label", "lease=cbx_lease") {
				t.Fatalf("provider and lease labels are required: %v", args)
			}
			if !findArg(args, "--label", "daemon=daemon-id") {
				t.Fatalf("daemon identity label is required: %v", args)
			}
		})
	}
}

func TestBuildRunArgsMountsCacheVolumes(t *testing.T) {
	b := testBackend(&recordingRunner{})
	b.cfg.Cache.Volumes = []core.CacheVolumeConfig{{Key: "npm", Path: "/var/cache/crabbox/npm"}}
	args, err := b.buildRunArgs(t.Context(), "cbx_lease", "blue-lobster", "name", "/tmp/inputs", "", "49152", []string{"127.0.0.1:5000:8080"}, false, daemonInfo{ID: "daemon-id"})
	if err != nil {
		t.Fatal(err)
	}
	if !findArg(args, "-v", "crabbox-cache-npm:/var/cache/crabbox/npm") {
		t.Fatalf("cache volume mount missing: %v", args)
	}
	if !findArg(args, "-p", "127.0.0.1:5000:8080") {
		t.Fatalf("exposed port mapping missing: %v", args)
	}
}

func TestDaemonModeComesFromSecurityOptions(t *testing.T) {
	for _, tc := range []struct {
		name       string
		options    []string
		wantRoot   bool
		configured bool
		wantErr    bool
	}{
		{"rootful", []string{"name=seccomp"}, false, false, false},
		{"rootless", []string{"name=seccomp", "name=rootless"}, true, true, false},
		{"mismatch fails closed", []string{"name=seccomp"}, false, true, true},
		{"plugins do not imply rootless", []string{"name=rootlesskit"}, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output, _ := jsonInfo(daemonInfo{ID: "daemon-id", SecurityOptions: tc.options})
			runner := &recordingRunner{run: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
				switch {
				case findArg(req.Args, "info"):
					return core.LocalCommandResult{Stdout: output}, nil
				case findArg(req.Args, "namespace", "ls"):
					return core.LocalCommandResult{Stdout: "default\n"}, nil
				}
				return core.LocalCommandResult{}, nil
			}}
			b := testBackend(runner)
			b.cfg.Containerd.Rootless = tc.configured
			info, err := b.verifyDaemon(t.Context())
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if err == nil && info.rootless() != tc.wantRoot {
				t.Fatalf("rootless=%v want %v", info.rootless(), tc.wantRoot)
			}
			if err != nil && tc.configured && !strings.Contains(err.Error(), "containerd.rootless: true") {
				t.Fatalf("a mode mismatch must name the working configuration: %v", err)
			}
		})
	}
}

// nerdctl reports image config labels under Config.Labels; reading the top
// level made the krun/TSI refusal unreachable.
func TestImageLabelReadsConfigLabels(t *testing.T) {
	for _, tc := range []struct {
		name        string
		inspect     string
		want        string
		wantRefusal bool
	}{
		{"passt label", `{"Id":"sha256:a","Config":{"Labels":{"crabbox.krun.network":"passt"}}}`, "passt", true},
		{"no label", `{"Id":"sha256:a","Config":{"Labels":{}}}`, "", false},
		{"top-level label is not the contract", `{"Id":"sha256:a","Labels":{"crabbox.krun.network":"passt"}}`, "", false},
		{"untagged index without config", `{"Id":"","RepoTags":["<untagged>"]}`, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &recordingRunner{run: func(core.LocalCommandRequest) (core.LocalCommandResult, error) {
				return core.LocalCommandResult{Stdout: tc.inspect}, nil
			}}
			b := testBackend(runner)
			b.cfg.Containerd.Runtime = "krun"
			b.cfg.Containerd.KrunNetwork = "tsi"
			got, err := b.imageLabel(t.Context(), "crabbox.krun.network")
			if err != nil || got != tc.want {
				t.Fatalf("label=%q err=%v want %q", got, err, tc.want)
			}
			err = b.refuseUnsupportedImageProfile(t.Context())
			if (err != nil) != tc.wantRefusal {
				t.Fatalf("refusal=%v want %v", err, tc.wantRefusal)
			}
			if tc.wantRefusal && !strings.Contains(err.Error(), "krunNetwork: passt") {
				t.Fatalf("refusal must name the working configuration: %v", err)
			}
		})
	}
}

// The narrow seccomp profile applies only to browser and desktop leases; a
// missing profile must not fail doctor on every plain lease.
func TestDoctorSkipsSeccompProfileWithoutBrowserOrDesktop(t *testing.T) {
	runner := &recordingRunner{run: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		switch {
		case findArg(req.Args, "--version"):
			return core.LocalCommandResult{Stdout: "nerdctl version 2.4.1"}, nil
		case findArg(req.Args, "info"):
			output, _ := jsonInfo(daemonInfo{ID: "daemon-id", Namespace: "default"})
			return core.LocalCommandResult{Stdout: output}, nil
		case findArg(req.Args, "namespace", "ls"):
			return core.LocalCommandResult{Stdout: "default\n"}, nil
		case findArg(req.Args, "ps"):
			return core.LocalCommandResult{}, nil
		}
		return core.LocalCommandResult{}, nil
	}}
	result, err := testBackend(runner).Doctor(t.Context(), core.DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range result.Checks {
		if check.Check == "seccomp-profile" && check.Status == "failed" {
			t.Fatalf("a plain lease reported a seccomp-profile failure: %+v", check)
		}
	}
}

func jsonInfo(info daemonInfo) (string, error) {
	encoded := `{"ID":"` + info.ID + `","Namespace":"default","SecurityOptions":[`
	for i, option := range info.SecurityOptions {
		if i > 0 {
			encoded += ","
		}
		encoded += `"` + option + `"`
	}
	return encoded + `]}`, nil
}

func TestRemoteTransportCarriesNoKeyMaterial(t *testing.T) {
	b := testBackend(&recordingRunner{})
	b.cfg.Containerd.SSHHost = "containerd.example.test"
	b.cfg.Containerd.SSHUser = "operator"
	b.cfg.Containerd.SSHKey = "/home/operator/.ssh/id_ed25519"
	argv := b.runner().remoteArgv(quoteArgv([]string{"run", "-d", "image"}))
	joined := strings.Join(argv, " ")
	for _, want := range []string{"BatchMode=yes", "StrictHostKeyChecking=accept-new", "ConnectTimeout=10", "-i /home/operator/.ssh/id_ed25519", "IdentitiesOnly=yes", "operator@containerd.example.test"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %q", want, joined)
		}
	}
	for _, forbidden := range []string{"PRIVATE KEY", "BEGIN OPENSSH"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("key material leaked onto argv: %q", joined)
		}
	}
}

func TestLeaseSSHTargetUsesTheContainerUser(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	b := testBackend(&recordingRunner{})
	b.cfg.Containerd.User = "crabbox"
	b.cfg.SSHUser = "operator"
	target, err := b.sshTargetForPort(t.Context(), "cbx_lease", "49152")
	if err != nil {
		t.Fatal(err)
	}
	if target.User != "crabbox" {
		t.Fatalf("target user=%q want the in-container user crabbox (not the operator SSH user)", target.User)
	}
	if target.Port != "49152" || target.Host != "127.0.0.1" || target.TargetOS != core.TargetLinux {
		t.Fatalf("target=%+v", target)
	}
}

func TestClaimScopeComponents(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*core.Config)
		want   string
	}{
		{"default", nil, "default/crun/overlayfs/direct/off/local"},
		{"rootless", func(c *core.Config) { c.Containerd.Rootless = true }, "default/crun/overlayfs/rootless/direct/off/local"},
		{"host", func(c *core.Config) { c.Containerd.SSHHost = "containerd.example.test" }, "default/crun/overlayfs/direct/off/containerd.example.test"},
		{"incomplete namespace", func(c *core.Config) { c.Containerd.Namespace = "" }, ""},
		{"incomplete runtime", func(c *core.Config) { c.Containerd.Runtime = "" }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testBackend(&recordingRunner{}).cfg
			if tc.mutate != nil {
				tc.mutate(&cfg)
			}
			if got := (Provider{}).ClaimScope(cfg); got != tc.want {
				t.Fatalf("ClaimScope=%q want %q", got, tc.want)
			}
		})
	}
}

func TestRequestedCapabilityRefusals(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*core.Config)
		wantErr bool
	}{
		{"actions runner with direct init", func(c *core.Config) { c.Actions.RunnerLabels = []string{"self-hosted"} }, true},
		{"actions runner with systemd init", func(c *core.Config) {
			c.Actions.RunnerLabels = []string{"self-hosted"}
			c.Containerd.InitMode = "systemd"
		}, false},
		{"criu on rootless crun", func(c *core.Config) {
			c.Containerd.CRIU = "on"
			c.Containerd.Rootless = true
		}, true},
		{"criu on rootless krun accepted", func(c *core.Config) {
			c.Containerd.CRIU = "on"
			c.Containerd.Rootless = true
			c.Containerd.Runtime = "krun"
		}, false},
		{"btrfs rootless", func(c *core.Config) {
			c.Containerd.Snapshotter = "btrfs"
			c.Containerd.Rootless = true
		}, true},
		{"krun nvidia", func(c *core.Config) {
			c.Containerd.Runtime = "krun"
			c.Containerd.GPU = "nvidia"
		}, true},
		{"krun drm", func(c *core.Config) {
			c.Containerd.Runtime = "krun"
			c.Containerd.GPU = "drm"
		}, true},
		{"virtio without krun", func(c *core.Config) { c.Containerd.GPU = "virtio" }, true},
		{"krun gpu low memory", func(c *core.Config) {
			c.Containerd.Runtime = "krun"
			c.Containerd.GPU = "virtio"
			c.Containerd.Memory = "2g"
		}, true},
		{"krun gpu 4g total leaves less than 4 GiB guest RAM", func(c *core.Config) {
			c.Containerd.Runtime = "krun"
			c.Containerd.GPU = "virtio"
			c.Containerd.Memory = "4g"
		}, true},
		{"krun gpu 4352m meets the guest floor", func(c *core.Config) {
			c.Containerd.Runtime = "krun"
			c.Containerd.GPU = "virtio"
			c.Containerd.Memory = "4352m"
		}, false},
		{"krun gpu sufficient memory", func(c *core.Config) {
			c.Containerd.Runtime = "krun"
			c.Containerd.GPU = "virtio"
			c.Containerd.Memory = "8g"
		}, false},
		{"krun desktop accepted", func(c *core.Config) {
			c.Containerd.Runtime = "krun"
			c.Desktop = true
		}, false},
		{"krun browser accepted", func(c *core.Config) {
			c.Containerd.Runtime = "krun"
			c.Browser = true
		}, false},
		{"browser without a seccomp profile", func(c *core.Config) {
			c.Browser = true
			c.Containerd.SeccompProfile = ""
		}, true},
		{"browser with the narrow profile", func(c *core.Config) { c.Browser = true }, false},
		{"nested on runc accepted", func(c *core.Config) { c.Containerd.Nested = "on" }, false},
		{"rootless nvidia accepted", func(c *core.Config) {
			c.Containerd.Rootless = true
			c.Containerd.GPU = "nvidia"
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testBackend(&recordingRunner{}).cfg
			if tc.mutate != nil {
				tc.mutate(&cfg)
			}
			err := validateRequestedCapabilities(cfg)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if tc.wantErr && !strings.Contains(err.Error(), ";") && !strings.Contains(err.Error(), "use ") && !strings.Contains(err.Error(), "set ") && !strings.Contains(err.Error(), "install ") && !strings.Contains(err.Error(), "raise ") {
				t.Fatalf("a refusal must name a working configuration: %v", err)
			}
		})
	}
}

func TestValidateConfigEnums(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*core.Config)
		wantErr bool
	}{
		{"valid", nil, false},
		{"bad runtime", func(c *core.Config) { c.Containerd.Runtime = "runc2" }, true},
		{"bad snapshotter", func(c *core.Config) { c.Containerd.Snapshotter = "zfs" }, true},
		{"bad gpu", func(c *core.Config) { c.Containerd.GPU = "cuda" }, true},
		{"empty namespace", func(c *core.Config) { c.Containerd.Namespace = "" }, true},
		{"empty image", func(c *core.Config) { c.Containerd.Image = "" }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testBackend(&recordingRunner{}).cfg
			if tc.mutate != nil {
				tc.mutate(&cfg)
			}
			err := validateConfigEnums(cfg)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestKrunRAMMiB(t *testing.T) {
	for _, tc := range []struct {
		memory string
		want   int
		err    error
	}{
		{"8g", 8192 - 256, nil},
		{"4096m", 4096 - 256, nil},
		{"256m", 0, errors.New("no guest RAM")},
	} {
		got, err := krunRAMMiB(tc.memory, 256)
		if tc.err != nil {
			if err == nil {
				t.Fatalf("%s: expected an error", tc.memory)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Fatalf("%s: got %d err=%v want %d", tc.memory, got, err, tc.want)
		}
	}
}

func TestProviderSpecHasNoNativeCheckpointClaimYet(t *testing.T) {
	spec := (Provider{}).Spec()
	for _, feature := range []core.Feature{core.FeatureCheckpoint, core.FeatureFork, core.FeatureRestore} {
		if spec.Features.Has(feature) {
			t.Fatalf("%s must not be declared until the checkpoint lifecycle exists", feature)
		}
	}
	if !spec.Features.Has(core.FeatureSSH) || !spec.Features.Has(core.FeatureClaimExec) {
		t.Fatalf("SSH and claim-exec are part of the provider contract: %v", spec.Features)
	}
}

func TestContainerdRegistersWithoutAliasCollision(t *testing.T) {
	provider, err := core.ProviderFor("containerd")
	if err != nil || provider.Spec().Name != providerName {
		t.Fatalf("containerd did not resolve: %v", err)
	}
	alias, err := core.ProviderFor("nerdctl")
	if err != nil || alias.Spec().Name != providerName {
		t.Fatalf("nerdctl alias did not resolve to containerd: %v", err)
	}
	container, err := core.ProviderFor("container")
	if err != nil || container.Spec().Name != "local-container" {
		t.Fatalf("container must keep resolving to local-container: %v", err)
	}
}
