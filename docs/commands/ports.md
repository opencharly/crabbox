# ports

`crabbox ports` bridges provider-native port publishing for an existing Crabbox
lease. It resolves a Crabbox-owned lease id or slug first, then asks the active
provider to list, publish, or unpublish ports.

```sh
crabbox ports --provider docker-sandbox --id blue-box
crabbox ports --provider docker-sandbox --id blue-box --publish 3000
crabbox ports --provider docker-sandbox --id blue-box --unpublish 41000:3000
crabbox ports --provider docker-sandbox --id blue-box --json
crabbox ports --provider codesandbox --id web-box --publish 3000 --json
crabbox ports --provider containerd --id warm-crab --publish 3000
crabbox ports --provider containerd --id warm-crab --publish 41000:3000
crabbox ports --provider containerd --id warm-crab --unpublish 3000
```

## Lease Resolution

`--id` is required. It accepts a Crabbox lease id or active slug. Providers only
act on Crabbox-owned leases; raw user-created sandboxes are rejected.

For `provider=docker-sandbox`, Crabbox resolves the local `dsbx_...` claim and
then calls `sbx ports <sandbox-name> ...`.

For `provider=codesandbox`, Crabbox resolves the local `csbx_...` claim,
verifies the remote CodeSandbox ownership tag, then asks the CodeSandbox SDK
bridge for open ports or provider-owned host URLs.

## Flags

```text
--id <lease-id-or-slug>
--provider <name>
--publish <spec>      publish a port mapping; repeatable
--unpublish <spec>    unpublish a port mapping; repeatable
--json                print provider-native listing output as JSON
```

`--publish` and `--unpublish` cannot be combined in the same call.

For Docker Sandbox, the spec format mirrors `sbx ports`:

```text
[[HOST_IP:]HOST_PORT:]SANDBOX_PORT[/PROTOCOL]
```

Examples:

- `3000` lets Docker Sandbox allocate an ephemeral host port for sandbox port
  `3000`.
- `41000:3000` binds host port `41000` to sandbox port `3000`.
- `127.0.0.1:41000:3000/tcp4` restricts the published port to IPv4 loopback.

For CodeSandbox, the spec is only the sandbox port number:

```text
SANDBOX_PORT
```

`--publish 3000` waits for port `3000` in the sandbox and prints the
CodeSandbox SDK host URL. With `--json`, CodeSandbox returns objects with
`port`, `host`, and `url` fields. `--unpublish` is not supported for
CodeSandbox because the SDK observes ports owned by running processes rather
than exposing a close-port operation; stop the process inside the sandbox.

For containerd, the spec is a loopback forward:

```text
[LOCAL_PORT:]CONTAINER_PORT
```

- `3000` chooses a free operator-side loopback port for container port `3000`
  and prints the mapping.
- `41000:3000` binds operator loopback `41000` to container port `3000`.

`nerdctl` cannot add or remove a published port on a running container, so the
provider does not ask it to. `--publish` starts a detached owner process that
runs `ssh -L 127.0.0.1:LOCAL_PORT:127.0.0.1:CONTAINER_PORT` over the lease's SSH
endpoint and records its pid and an advisory lock under the Crabbox state
directory. The owner outlives the `ports` command, `--unpublish` stops it, and
`crabbox stop` reaps any forward the lease still owns. `ports` lists live
forwards beside the `nerdctl port` mappings. If a forward's teardown cannot be
proven, its record is retained and reported as failed rather than deleted;
`--unpublish` clears it once nothing is listening on its operator-side port.

The port lives **inside the lease**, never on the operator's loopback. On a
remote containerd host the forward rides the same SSH tunnel the lease already
uses; nothing probes the remote host's loopback from the operator. Publishing a
port nothing listens on inside the lease is refused, and the error names the
lease side and the containerd host it runs on.

Creating the lease with `--expose <port>` is the alternative: `nerdctl`
publishes that port on the daemon's loopback at container start, and `ports`
lists it without an owned forward. `--expose` cannot be changed afterwards.

## Provider Support

This command is provider-opt-in. Providers without a native port-publishing or
URL bridge fail clearly instead of guessing. Supported providers:

- `docker-sandbox` — native `sbx ports` list, publish, and unpublish.
- `codesandbox` — SDK open-port listing and host URL publishing.
- `containerd` — `nerdctl port` listing plus owned SSH local forwards
  (`--publish`/`--unpublish`) on a Unix operator.

## See also

- [`cp`](cp.md) - copy files between host and a delegated sandbox.
- [`stop`](stop.md) - remove the lease when you are done.
- [Docker Sandbox provider](../providers/docker-sandbox.md) - provider-specific
  notes and `sbx` command mapping.
- [CodeSandbox provider](../providers/codesandbox.md) - provider-specific
  host URL and preview token notes.
