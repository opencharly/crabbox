package containerd

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
	_ "github.com/openclaw/crabbox/internal/providers/localcontainer"
)

type recordingRunner struct {
	mu    sync.Mutex
	calls []core.LocalCommandRequest
	run   func(core.LocalCommandRequest) (core.LocalCommandResult, error)
}

func (r *recordingRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	// The SSH forward runs on its own goroutine; keep the recorder race-free.
	r.mu.Lock()
	r.calls = append(r.calls, req)
	r.mu.Unlock()
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
		forwards: map[forwardKey]*sshForward{},
		acquired: map[string]core.LeaseTarget{},
	}
}

// testBackendWithRunner builds the same backend over any CommandRunner, so a
// test can model a long-lived ssh child instead of an instant return.
func testBackendWithRunner(runner core.CommandRunner) *backend {
	b := testBackend(&recordingRunner{})
	b.rt.Exec = runner
	return b
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
			name: "host volumes",
			mutate: func(c *core.Config) {
				c.Containerd.Volumes = []string{"/host/data:/data", "cachevol:/cache:ro"}
			},
			want: [][]string{
				{"-v", "/host/data:/data"},
				{"-e", "CRABBOX_HOST_VOLUME_PATH_0=/data"},
				{"-v", "cachevol:/cache:ro"},
				{"-e", "CRABBOX_HOST_VOLUME_PATH_1=/cache"},
			},
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

func TestBuildRunArgsRejectsAMalformedVolume(t *testing.T) {
	b := testBackend(&recordingRunner{})
	b.cfg.Containerd.Volumes = []string{"no-destination"}
	_, err := b.buildRunArgs(t.Context(), "cbx_lease", "blue-lobster", "name", "/tmp/inputs", "", "49152", nil, false, daemonInfo{ID: "daemon-id"})
	if err == nil || !strings.Contains(err.Error(), "source:destination") {
		t.Fatalf("err=%v, want a named malformed-volume refusal", err)
	}
}

func TestImageLabelReportsUnreadableInspectOutput(t *testing.T) {
	runner := &recordingRunner{run: func(core.LocalCommandRequest) (core.LocalCommandResult, error) {
		return core.LocalCommandResult{Stdout: "not-json"}, nil
	}}
	if _, err := testBackend(runner).imageLabel(t.Context(), "crabbox.krun.network"); err == nil {
		t.Fatal("malformed image inspect JSON must be reported, not silently tolerated")
	}
}

func TestImageLabelToleratesAnImageThatIsNotLocalYet(t *testing.T) {
	runner := &recordingRunner{run: func(core.LocalCommandRequest) (core.LocalCommandResult, error) {
		return core.LocalCommandResult{Stderr: "FATA[0000] no such image: example:missing"}, errors.New("exit status 1")
	}}
	label, err := testBackend(runner).imageLabel(t.Context(), "crabbox.krun.network")
	if err != nil || label != "" {
		t.Fatalf("label=%q err=%v, want the not-local-yet image tolerated", label, err)
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

// A remote call must execute nerdctl on the far side, not a bare flag: the
// ssh argv carries one command string, so the binary name must be in it.
func TestRemoteTransportInvokesNerdctl(t *testing.T) {
	runner := &recordingRunner{}
	b := testBackend(runner)
	b.cfg.Containerd.SSHHost = "containerd.example.test"
	b.cfg.Containerd.SSHUser = "operator"
	b.cfg.Containerd.SSHKey = "/home/operator/.ssh/id_ed25519"
	if _, err := b.runner().run(t.Context(), []string{"-n", "default", "info"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls=%d want 1", len(runner.calls))
	}
	req := runner.calls[0]
	if req.Name != "ssh" {
		t.Fatalf("remote transport ran %q, want ssh", req.Name)
	}
	remote := req.Args[len(req.Args)-1]
	if !strings.HasPrefix(remote, "nerdctl ") {
		t.Fatalf("remote command %q does not invoke nerdctl", remote)
	}
	if !strings.Contains(remote, "info") {
		t.Fatalf("remote command %q lost the original argv", remote)
	}
	for _, forbidden := range []string{"PRIVATE KEY", "BEGIN OPENSSH"} {
		if strings.Contains(remote, forbidden) {
			t.Fatalf("key material leaked onto the remote command: %q", remote)
		}
	}
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
		{"krun desktop accepted on the prepared desktop image", func(c *core.Config) {
			c.Containerd.Runtime = "krun"
			c.Containerd.Image = "ghcr.io/opencharly/pod-crabbox-desktop"
			c.Desktop = true
		}, false},
		{"krun browser accepted on the prepared browser image", func(c *core.Config) {
			c.Containerd.Runtime = "krun"
			c.Containerd.Image = "ghcr.io/opencharly/pod-crabbox-browser"
			c.Browser = true
		}, false},
		{"browser without a seccomp profile", func(c *core.Config) {
			c.Containerd.Image = "ghcr.io/opencharly/pod-crabbox-browser"
			c.Browser = true
			c.Containerd.SeccompProfile = ""
		}, true},
		{"browser with the narrow profile", func(c *core.Config) {
			c.Containerd.Image = "ghcr.io/opencharly/pod-crabbox-browser"
			c.Browser = true
		}, false},
		{"desktop on a stock image", func(c *core.Config) { c.Desktop = true }, true},
		{"browser on a stock image", func(c *core.Config) { c.Browser = true }, true},
		{"code on a stock image", func(c *core.Config) { c.Code = true }, true},
		{"tailscale on a stock image", func(c *core.Config) { c.Tailscale.Enabled = true }, true},
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
		{"bad criu value", func(c *core.Config) { c.Containerd.CRIU = "bogus" }, true},
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

// The features are declared only because the lifecycle that backs them exists
// (the seam-1 guard). The compiler's interface assertions below are the
// control: removing any implementation breaks this test's build.
func TestProviderSpecDeclaresTheCheckpointLifecycle(t *testing.T) {
	spec := (Provider{}).Spec()
	for _, feature := range []core.Feature{core.FeatureCheckpoint, core.FeatureFork, core.FeatureRestore} {
		if !spec.Features.Has(feature) {
			t.Fatalf("%s must be declared with the checkpoint lifecycle: %v", feature, spec.Features)
		}
	}
	if !spec.Features.Has(core.FeatureSSH) || !spec.Features.Has(core.FeatureClaimExec) {
		t.Fatalf("SSH and claim-exec are part of the provider contract: %v", spec.Features)
	}
	var provider any = Provider{}
	if _, ok := provider.(core.NativeCheckpointProvider); !ok {
		t.Fatal("containerd must implement NativeCheckpointProvider")
	}
	if _, ok := provider.(core.NativeCheckpointLifecycleProvider); !ok {
		t.Fatal("containerd must implement NativeCheckpointLifecycleProvider")
	}
	if _, ok := provider.(core.NativeCheckpointForkProvider); !ok {
		t.Fatal("containerd must implement NativeCheckpointForkProvider")
	}
	if _, ok := provider.(core.NativeCheckpointRestoreProvider); !ok {
		t.Fatal("containerd must implement NativeCheckpointRestoreProvider")
	}
}

func TestContainerdRegistersWithoutAnInventedAlias(t *testing.T) {
	provider, err := core.ProviderFor("containerd")
	if err != nil || provider.Spec().Name != providerName {
		t.Fatalf("containerd did not resolve: %v", err)
	}
	if aliases := provider.Spec().Aliases; len(aliases) != 0 {
		t.Fatalf("containerd must not carry an invented alias: %v", aliases)
	}
	if _, err := core.ProviderFor("nerdctl"); err == nil {
		t.Fatal("nerdctl must not resolve as a provider: it is the CLI tool the provider invokes, not an alias")
	}
	container, err := core.ProviderFor("container")
	if err != nil || container.Spec().Name != "local-container" {
		t.Fatalf("container must keep resolving to local-container: %v", err)
	}
}

func TestConfiguredFeaturesFollowTheConfiguredImage(t *testing.T) {
	optional := []core.Feature{core.FeatureDesktop, core.FeatureBrowser, core.FeatureCode, core.FeatureTailscale}
	cfg := testBackend(&recordingRunner{}).cfg
	for _, feature := range optional {
		if (Provider{}).ConfiguredFeatures(cfg).Has(feature) {
			t.Fatalf("%s must not be admitted on the stock image %q", feature, cfg.Containerd.Image)
		}
	}
	for image, feature := range map[string]core.Feature{
		"ghcr.io/opencharly/pod-crabbox-desktop:v1":   core.FeatureDesktop,
		"ghcr.io/opencharly/pod-crabbox-browser":      core.FeatureBrowser,
		"pod-crabbox-code@sha256:deadbeef":            core.FeatureCode,
		"registry.example:5000/pod-crabbox-tailscale": core.FeatureTailscale,
	} {
		cfg.Containerd.Image = image
		if !(Provider{}).ConfiguredFeatures(cfg).Has(feature) {
			t.Fatalf("%s must be admitted for image %q", feature, image)
		}
	}
}

func TestConfiguredImageServes(t *testing.T) {
	for _, tc := range []struct {
		reference, prepared string
		want                bool
	}{
		{"ghcr.io/opencharly/pod-crabbox-desktop", "pod-crabbox-desktop", true},
		{"ghcr.io/opencharly/pod-crabbox-desktop:v1", "pod-crabbox-desktop", true},
		{"pod-crabbox-desktop@sha256:deadbeef", "pod-crabbox-desktop", true},
		{"registry.example:5000/pod-crabbox-desktop:1", "pod-crabbox-desktop", true},
		{"ghcr.io/opencharly/pod-crabbox-desktop-extra", "pod-crabbox-desktop", false},
		{"docker.io/library/ubuntu:24.04", "pod-crabbox-desktop", false},
	} {
		if got := configuredImageServes(tc.reference, tc.prepared); got != tc.want {
			t.Fatalf("configuredImageServes(%q, %q)=%v want %v", tc.reference, tc.prepared, got, tc.want)
		}
	}
}
