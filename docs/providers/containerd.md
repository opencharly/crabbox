# containerd Provider

Read this when you:

- choose `provider: containerd`;
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
crabbox warmup --provider containerd --containerd-image docker.io/library/ubuntu:24.04 --slug ct-smoke
crabbox ssh --provider containerd --id ct-smoke
crabbox doctor --provider containerd
crabbox stop --provider containerd ct-smoke
```

A rootless daemon is not inferred from the socket: the provider verifies the
reached daemon's mode and fails closed on a mismatch, so name it explicitly
(`CRABBOX_CONTAINERD_ROOTLESS=true` or `containerd.rootless: true`):

```sh
crabbox run --provider containerd --containerd-rootless -- sh -c 'uname -s; id -un'
crabbox doctor --provider containerd --containerd-rootless
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
| `criu` | `off` / `auto` (`on` and `in-container` land with opencharly/crabbox#23) | `auto` |
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
| checkpoints (`containerd-commit`), fork, restore | `nerdctl commit` of the root filesystem plus a state-volume clone; a running source is paused and `/etc/machine-id` is emptied (restored afterwards); GNU `cp` required | any image with GNU coreutils; `pod-crabbox-k3s` for the k3s snapshot/restore use case |

## Refusals — each names the mechanism and a working configuration

Each refusal names the mechanism and a configuration that works. Nothing is
silently downgraded.

| Request | Mechanism | Working configuration |
|---|---|---|
| Actions runner + `initMode: direct` | the runner installs a systemd unit; direct mode runs the bootstrap as PID 1 | `initMode: systemd` |
| `snapshotter: btrfs` + rootless | a rootless user namespace cannot mount the btrfs device (`btrfs.go:240-248`, NEEDS UPSTREAM) | `snapshotter: native` on a btrfs data root, or `overlayfs` |
| `runtime: krun` + `gpu: nvidia` | the render-server sandbox dev-binds only `/dev/dri` (`handlers/krun.c:699-703`, NEEDS UPSTREAM) | crun/runc + CDI, or `gpu: virtio` on AMD/Intel |
| `runtime: krun` + `gpu: drm` | krun takes GPUs through `krun.gpu_flags`, never `--device /dev/dri` | `gpu: virtio`, or crun/runc + `gpu: drm` |
| `gpu: virtio` with crun/runc | virtio GPU flags are a krun handler annotation | `runtime: krun`, or `gpu: drm`/`nvidia` |
| krun GPU + guest RAM below 4 GiB | the guest OOM-kills Vulkan compute at 1 GiB; the guest sees `memory` minus `krunRamHeadroomMiB` | raise `memory` to at least 4 GiB plus `krunRamHeadroomMiB` (4352m with the default 256 MiB headroom) |
| `--desktop` / `--browser` / `--code` / `--tailscale` on an image that does not carry it | the capability lives in the prepared image's layers, not the base SSH contract | set `containerd.image` to the matching `pod-crabbox-desktop` / `-browser` / `-code` / `-tailscale` image |
| browser/desktop without the profile | stock seccomp breaks Chromium's sandbox | install the profile from `CONTAINERD.md` §3.1 |
| image label `crabbox.krun.network=passt` + `krunNetwork: tsi` | TSI collapses the pod netns and breaks CRIU's loopback probe | `krunNetwork: passt` |

At acquisition the provider also refuses a namespace missing on the reached
daemon. `criu: auto` narrows to `off` in the lease output and in `crabbox doctor`;
`criu: on` and `criu: in-container` fail closed until the process-checkpoint seam
(opencharly/crabbox#23) lands.

## Checkpoints

Generic checkpoints (repo metadata, logs, artifacts) work as they do for every
provider. The native primitive is the `containerd-commit` kind, opt in with
`--mode native` (or `--strategy image`):

- **Create** — `crabbox checkpoint create --id <lease> --mode native`. A running
  source has `/etc/machine-id` saved and emptied over SSH, is paused, committed
  with `nerdctl commit` (a persistent `CMD`, no `LABEL`), and its state volume is
  copied into `crabbox-cp-<checkpoint-id>-state` by a helper container. The
  source is unpaused and its machine-id restored in a finally step. A stopped
  source is a cold capture: no pause and no SSH step, and the empty machine-id is
  produced in a derived image. The image must carry GNU `cp`; busybox `cp` drops
  `trusted.overlay.*` xattrs and is refused before capture.
- **Verify** — `crabbox checkpoint inspect <id> --verify` checks the recorded tag
  still resolves to the recorded digest and that the recorded daemon scope
  (host, daemon id, namespace, runtime, snapshotter, rootless) still matches.
  Drift exits 7.
- **Delete** — removes the verified tag and the state-volume clone. A tag that
  now points at another image is refused.
- **Fork** — a new lease from the committed image, with the captured state
  volume cloned into the new lease's volume and fresh lease labels. The host
  volumes present when the checkpoint was created are recorded and replayed; a
  checkpoint created without them does not add any.
- **Restore** — `crabbox checkpoint restore <id> --id <lease>` is bound to the
  lease the checkpoint was taken from (a different `--id` exits 7 before any
  daemon call). It keeps the lease id, claim, SSH key and published ports,
  re-clones the state volume, recreates the container from the image, and
  reports convergence after the readiness hook. A missing state-volume clone is
  refused before the live lease is touched.

`containerd-criu` (process checkpoints via `--strategy process`) is a later
seam and is not offered yet.

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
mocked. Set `CRABBOX_LIVE_CONTAINERD_STRICT=1` to turn a skip into a non-zero
exit, and `CRABBOX_LIVE_CONTAINERD_IMAGES=0` to run only the base row.

## Related docs

- [Provider backends](../provider-backends.md) — the contract reference.
- [Authoring a provider](../features/provider-authoring.md) — the walkthrough.
- [Local Container](local-container.md) — the Docker-compatible sibling.
- [Provider reference](README.md) — the full catalog.
