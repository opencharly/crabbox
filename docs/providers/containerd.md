# containerd Provider

Read this when you:

- choose `provider: containerd` (alias `nerdctl`);
- run Crabbox leases through containerd on the local machine or a remote Linux
  host;
- change `internal/providers/containerd` or the shared
  `internal/providers/shared/containerbootstrap` script.

containerd is an SSH-lease provider: Crabbox starts one labeled container with
`nerdctl`, publishes its SSH port on the operator's loopback (a local `ssh -L`
tunnel when the daemon is remote), syncs the checkout over SSH, runs the command
with the normal SSH executor, and removes the container on release. Core owns
sync, run, logs, artifacts, checkpoints from repo metadata, hydration and
rendering exactly as it does for `local-container`.

**Every feature rides SSH.** The provider never runs `nerdctl exec`; a `krun`
guest has no exec at all. Guest commands go through the lease's SSH target.

## Prerequisites

The daemon, rootful/rootless setup, namespace model, runtime handlers, snapshotter
and CRIU requirements are defined in `CONTAINERD.md` §2 (rootful) and §4
(rootless) in `opencharly/opencharly`. The image contract (sshd on 2222, the
`/crabbox` inputs, the readiness hook) is defined in `CONTAINERD-IMAGES.md` §2.

- `nerdctl` on the containerd host, and a reachable containerd daemon.
- The runtime binary named by `containerd.runtime` (default `/usr/bin/<runtime>`).
- Browser and desktop leases need the narrow seccomp profile from
  `CONTAINERD.md` §3.1; a stock profile breaks Chromium's sandbox.
- Remote mode uses SSH to the containerd host. The configured key is a **path**;
  key material never enters configuration or argv. Remote mode also needs
  `python3` on the containerd host to allocate a free published port.

## Quick start

Local daemon:

```sh
crabbox run --provider containerd -- sh -c 'uname -s; id -un'
crabbox warmup --provider nerdctl --containerd-image docker.io/library/ubuntu:24.04 --slug ct-smoke
crabbox ssh --provider nerdctl --id ct-smoke
crabbox doctor --provider containerd
crabbox stop --provider nerdctl ct-smoke
```

Remote containerd host:

```sh
crabbox run --provider containerd \
  --containerd-ssh-host containerd.example.test \
  --containerd-ssh-user operator \
  --containerd-ssh-key ~/.ssh/id_ed25519 \
  -- true
```

krun guest:

```sh
crabbox run --provider containerd \
  --containerd-runtime krun \
  --containerd-memory 8g --containerd-cpus 4 \
  -- uname -a
```

## Configuration

Every key except the creation-only `volumes` list has a
`CRABBOX_CONTAINERD_<KEY>` environment variable and a `--containerd-<key>` flag.
Flags win over the environment, the repository file, then the user file.

| Key | Values | Default |
|---|---|---|
| `address` | socket | `/run/containerd/containerd.sock` |
| `sshHost` / `sshUser` / `sshPort` / `sshKey` | remote host; key **path** | empty / `root` / `22` / empty |
| `rootless` | bool (verified, fail closed) | `false` |
| `runtime` | `crun` / `krun` / `runc` | `crun` |
| `runtimePath` | absolute path | `/usr/bin/<runtime>` |
| `snapshotter` | `overlayfs` / `native` / `fuse-overlayfs` / `btrfs` | `overlayfs` |
| `namespace` | name | `default` |
| `image` | OCI reference | `docker.io/library/ubuntu:24.04` (stock) |
| `user` / `workRoot` / `network` | | `crabbox` / `/work/crabbox` / `bridge` |
| `initMode` | `direct` / `systemd` | `direct` |
| `nested` | `off` / `on` | `off` |
| `stateVolume` | `auto` / `off` | `auto` |
| `gpu` | `off` / `drm` / `nvidia` / `virtio` | `off` |
| `criu` | `off` / `on` / `auto` / `in-container` | `auto` |
| `cpus` / `memory` | int / size | `0` / empty |
| `seccompProfile` | path | `/etc/containerd/seccomp/crabbox-userns.json` |
| `krunGpuFlags` | int | `962` (`1410` is GL-only; never `1283`) |
| `krunRamHeadroomMiB` | int | `256` |
| `krunNestedVirt` / `krunNetwork` / `krunKernel` | bool / `tsi` or `passt` / `stock` or `image` | `false` / `tsi` / `stock` |
| `nodeAddress` | CIDR | `172.31.255.1/32` |
| `volumes` | repeatable flag only, creation only | — |

The provider derives the scope it records on a local claim from
`namespace/runtime/snapshotter[/rootless]/init/nested/host`. Resolve fails closed
when the configured socket reaches a different daemon ID, namespace or host than
the claim records.

## Features and the image each needs

The prepared runner images are `pod-crabbox*`; their contents and per-variant
layers are defined in `CONTAINERD-IMAGES.md` §2 and §5. Any stock image can run
the base SSH contract through the shared bootstrap script, but the feature rows
below need the named image because the capability lives in its layers.

| Feature | What the provider does | Image |
|---|---|---|
| SSH, sync, run, logs, artifacts | SSH lease; no exec | any Linux image with sshd or the shared bootstrap |
| cache-volume | keyed named volumes (`crabbox-cache-<key>`) mounted at each configured cache path | any image |
| desktop, browser, code | installs/starts the in-lease service over SSH; browser/desktop add the narrow profile on crun/runc | `pod-crabbox-desktop`, `pod-crabbox-browser`, `pod-crabbox-code` |
| tailscale | kernel mode adds `/dev/net/tun` and `NET_ADMIN`; metadata is read from in-lease state | `pod-crabbox-tailscale` |
| nested | `--privileged`, `/dev/kmsg`, the state volume, `/dev/kvm` when present | `pod-crabbox-nested` |
| gpu `drm` | `/dev/dri` (`/dev/kfd` when present), video/render groups | any image with the userspace driver |
| gpu `nvidia` | CDI device `nvidia.com/gpu=all` plus the `files.d` mount | any image with the NVIDIA userspace |
| gpu `virtio` (krun) | `krun.gpu_flags` annotation; never `--device /dev/dri` | `pod-crabbox-gpu` |
| k3s snapshot/restore | generic checkpoints from repo metadata today; native checkpoint kinds are not offered yet (see below) | `pod-crabbox-k3s` |

## Refusals — measured limits only

Each refusal names the mechanism and a configuration that works. Nothing is
silently downgraded.

| Request | Mechanism | Working configuration |
|---|---|---|
| Actions runner + `initMode: direct` | the runner installs a systemd unit; direct mode runs the bootstrap as PID 1 | `initMode: systemd` |
| `criu: on` + rootless + crun/runc | init-userns gate for seccomp-filtered tasks, netns switch-back, `map_files` | `runtime: krun` (in-guest) or `criu: in-container` |
| `snapshotter: btrfs` + rootless | a rootless user namespace cannot mount the btrfs device (`btrfs.go:240-248`, NEEDS UPSTREAM) | `snapshotter: native` on a btrfs data root, or `overlayfs` |
| `runtime: krun` + `gpu: nvidia` | the render-server sandbox dev-binds only `/dev/dri` (`handlers/krun.c:699-703`, NEEDS UPSTREAM) | crun/runc + CDI, or `gpu: virtio` on AMD/Intel |
| `runtime: krun` + `gpu: drm` | krun takes GPUs through `krun.gpu_flags`, never `--device /dev/dri` | `gpu: virtio`, or crun/runc + `gpu: drm` |
| `gpu: virtio` with crun/runc | virtio GPU flags are a krun handler annotation | `runtime: krun`, or `gpu: drm`/`nvidia` |
| krun GPU + `memory` below 4 GiB | the guest OOM-kills Vulkan compute at 1 GiB | raise `memory` (guest RAM is memory minus `krunRamHeadroomMiB`) |
| browser/desktop without the profile | stock seccomp breaks Chromium's sandbox | install the profile from `CONTAINERD.md` §3.1 |
| image label `crabbox.krun.network=passt` + `krunNetwork: tsi` | TSI collapses the pod netns and breaks CRIU's loopback probe | `krunNetwork: passt` |

At acquisition the provider also refuses a namespace missing on the reached
daemon, and reports `criu: auto` narrowing to `off` in the lease output and in
`crabbox doctor` — the automatic CRIU pre-flight belongs to the checkpoint seam
and is never implied.

## Checkpoints

Generic checkpoints (repo metadata, logs, artifacts) work as they do for every
provider. This provider does **not** yet offer a native `containerd-commit` image
checkpoint, the restore interface, or `containerd-criu`; declaring those features
before their lifecycle exists would be a false capability claim. They are the
next seam of the containerd work.

A native checkpoint, once it exists, is bound to its lease claim: the same
containerd daemon ID, namespace and host, and the owning repository. Another
operator on a different daemon or namespace cannot read or restore it, and a
deleted tag or snapshot volume is not silently recreated.

## Rootless CDI

The NVIDIA CDI spec is written rootful at `/var/run/cdi/nvidia.yaml`. A rootless
lease cannot see it; the working configuration is
`ln -sf /var/run/cdi/nvidia.yaml ~/.config/cdi/nvidia.yaml`. `crabbox doctor
--provider containerd` reports the missing spec with that remedy.

## Verification

```sh
crabbox providers describe containerd
crabbox doctor --provider containerd
scripts/live-containerd-smoke.sh
```

The live smoke runs the base row on a stock image and skips **visibly, by image
name**, for each `pod-crabbox*` image the host does not have yet. A host without
a reachable containerd daemon skips visibly; so does a rootless host whose CNI
`portmap` plugin cannot append its `iptables -m multiport` rule because the
kernel lacks `xt_multiport` (the host layer owns that prerequisite). Nothing is
mocked.

## Related docs

- [Provider backends](../provider-backends.md) — the contract reference.
- [Authoring a provider](../features/provider-authoring.md) — the walkthrough.
- [Local Container](local-container.md) — the Docker-compatible sibling.
- [Provider reference](README.md) — the full catalog.
