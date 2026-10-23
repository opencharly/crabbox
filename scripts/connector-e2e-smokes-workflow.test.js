import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";

const repoRoot = path.resolve(import.meta.dirname, "..");
const workflow = fs.readFileSync(
  path.join(repoRoot, ".github", "workflows", "connector-e2e-smokes.yml"),
  "utf8",
);

function runScript(step) {
  const marker = "        run: |\n";
  assert.ok(step.includes(marker), "step has a literal run block");
  const lines = step.slice(step.indexOf(marker) + marker.length).split("\n");
  const end = lines.findIndex((line) => line.trim() && !line.startsWith("          "));
  return lines.slice(0, end < 0 ? undefined : end)
    .map((line) => line.replace(/^ {10}/, "")).join("\n");
}

test("run block extraction stops at the next step or job and accepts EOF", () => {
  const block = "        run: |\n          echo fixture\n";
  for (const suffix of ["", "      - name: Next\n", "  next-job:\n"]) {
    assert.equal(runScript(block + suffix).trim(), "echo fixture");
  }
});

test("connector lifecycle gate runs on pull requests, main pushes, and manual dispatch only", () => {
  const trigger = workflow.slice(workflow.indexOf("\non:"), workflow.indexOf("permissions:"));
  assert.match(trigger, /pull_request:/);
  assert.match(trigger, /push:\s*\n\s+branches:\s*\n\s+- main/);
  assert.match(trigger, /workflow_dispatch:/);
  assert.doesNotMatch(
    workflow,
    /schedule:/,
    "the hermetic gate must not run on a schedule; tier 2 needs a maintainer credential policy first",
  );
});

test("connector lifecycle gate stays hermetic: no secret references anywhere", () => {
  assert.ok(
    !workflow.includes("secrets."),
    "workflow must not reference secrets; tier 1 of https://github.com/openclaw/crabbox/issues/944 is zero-credential",
  );
});

test("matrix rows do not fail fast and are time-bounded", () => {
  assert.match(workflow, /fail-fast:\s*false/);
  assert.match(workflow, /timeout-minutes: \$\{\{ matrix\.timeout-minutes \|\| 15 \}\}/);
  const localContainer = workflow.match(
    /- name: local-container\n([\s\S]*?)(?=\n {10}- name:)/,
  )?.[1];
  assert.ok(localContainer, "local-container row exists");
  assert.match(localContainer, /timeout-minutes: 40/);
  const containerd = workflow.match(
    /- name: containerd\n([\s\S]*?)(?=\n {10}- name:)/,
  )?.[1];
  assert.ok(containerd, "containerd row exists");
  assert.match(containerd, /timeout-minutes: 30/);
  const timeouts = [...workflow.matchAll(/^ {12}timeout-minutes: (\d+)$/gm)].map((m) => m[1]);
  assert.deepEqual(timeouts.sort(), ["30", "40"]);
});

test("SSH localhost asserts amd64 only on its hosted x64 lifecycle row", () => {
  const rows = [...workflow.matchAll(/^ {10}- name: ssh-localhost\n((?: {12}[^\n]*\n)*)/gm)];
  assert.equal(rows.length, 1, "keep one existing SSH localhost row");
  const row = rows[0][1];
  assert.ok(row.includes("runner: ${{ vars.CI_RUNNER_LABEL || 'opencharly' }}"), "ssh-localhost uses the exact general runner expression");
  assert.match(row, /^ {12}build-cli: true$/m);
  assert.match(
    row,
    /^ {12}smoke: CRABBOX_ARCH=amd64 CRABBOX_BIN="\$PWD\/bin\/crabbox" scripts\/live-ssh-localhost-smoke\.sh$/m,
  );
  assert.equal((workflow.match(/CRABBOX_ARCH/g) ?? []).length, 1);
  assert.match(workflow, /runs-on: \$\{\{ matrix\.runner \}\}/);
  assert.match(workflow, /run: \$\{\{ matrix\.smoke \}\}/);
  assert.match(workflow, /permissions:\n {2}contents: read\n\n/);
  assert.doesNotMatch(
    workflow,
    /pull_request_target:|self-hosted|^\s*(?:environment|services|container|continue-on-error):/m,
  );
});

test("container matrix rows select the Docker fleet through the org variable", () => {
  for (const name of ["firecracker-readiness", "ssh-localhost"]) {
    const block = workflow.match(
      new RegExp(`- name: ${name}\\n([\\s\\S]*?)(?=\\n {10}- name:)`),
    )?.[1];
    assert.ok(block, `${name} row exists`);
    assert.ok(block.includes("runner: ${{ vars.CI_RUNNER_LABEL || 'opencharly' }}"), `${name} uses the exact general runner expression`);
  }
  for (const name of ["docker-sandbox-trust-boundary", "local-container", "containerd"]) {
    const block = workflow.match(
      new RegExp(`- name: ${name}\\n([\\s\\S]*?)(?=\\n {10}- name:)`),
    )?.[1];
    assert.ok(block, `${name} row exists`);
    assert.ok(block.includes("runner: ${{ vars.CI_DOCKER_RUNNER_LABEL || 'opencharly-ubuntu' }}"), `${name} uses the exact Docker runner expression`);
  }
});

test("pull request runs cancel superseded attempts", () => {
  assert.match(workflow, /concurrency:\s*\n\s+group:/);
  assert.match(workflow, /cancel-in-progress:.*pull_request/);
});

test("the containerd row pins its tools, gates xt_multiport loudly, and rejects a skip", () => {
  const row = workflow.match(/- name: containerd\n([\s\S]*?)(?=\n {10}- name:)/)?.[1];
  assert.ok(row, "containerd row exists");
  assert.match(row, /CRABBOX_LIVE_PROVIDERS=containerd/);
  assert.match(row, /CRABBOX_LIVE_CONTAINERD_STRICT=1/);
  assert.match(row, /CRABBOX_LIVE_CONTAINERD_IMAGES=0/);
  assert.match(row, /sudo env/);

  const install = workflow
    .split("      - name: Install containerd, nerdctl, and CNI plugins\n")[1]
    ?.split("\n      - name:")[0];
  assert.ok(install, "containerd install step exists");
  assert.match(install, /NERDCTL_VERSION: 2\.4\.1\n/);
  assert.match(install, /NERDCTL_SHA256: f6bbf02fd8a299cbcbc71a0bad113c1b63b3f33689bbb7d51b0fa98c6d361d5e\n/);
  assert.match(install, /CNI_VERSION: 1\.9\.1\n/);
  assert.match(install, /CNI_SHA256: b98f74a0f8522f0a83867178729c1aa70f2158f90c45a2ca8fa791db1c76b303\n/);
  assert.match(install, /sudo apt-get install -y containerd runc/);
  assert.match(install, /systemctl enable --now containerd/);
  assert.match(install, /-C \/opt\/cni\/bin/);
  assert.match(install, /sha256sum --check --strict/);

  const preflight = workflow
    .split("      - name: Require the CNI portmap kernel extension\n")[1]
    ?.split("\n      - name:")[0];
  assert.ok(preflight, "xt_multiport preflight step exists");
  assert.match(preflight, /grep -qw multiport \/proc\/net\/ip_tables_matches/);
  assert.match(preflight, /::error::/);
  assert.match(preflight, /opencharly\/layer-containerd#5/);
  assert.match(preflight, /exit 1/);
  assert.doesNotMatch(preflight, /\|\| true/, "the preflight must not be suppressed");

  const reject = workflow
    .split("      - name: Reject a skipped containerd smoke\n")[1]
    ?.split("\n      - name:")[0];
  assert.ok(reject, "containerd skip-rejection step exists");
  assert.match(reject, /a skip is not a pass/);
  assert.match(reject, /skip:/);
  assert.match(reject, /exit 1/);
});

for (const scenario of [
  { name: "primary success", primary: "good", calls: 1, succeeds: true },
  { name: "partial transfer then official fallback", primary: "partial", fallback: "good", calls: 2, succeeds: true },
  { name: "both transfers fail", primary: "partial", fallback: "partial", calls: 2 },
  { name: "primary checksum mismatch does not fall back", primary: "corrupt", calls: 1 },
  { name: "fallback checksum mismatch", primary: "partial", fallback: "corrupt", calls: 2 },
]) {
  test(`rsync download: ${scenario.name}`, (t) => {
    const step = workflow.split("      - name: Install security-current rsync\n")[1]
      .split("\n      - name:")[0];
    assert.match(step, /RSYNC_VERSION: 3\.4\.4\n/);
    assert.match(step, /RSYNC_SHA256: bd88cf82fa653da32314fb229136407c5c90f80d1758d8f4b091767877d8fa96\n/);
    const script = runScript(step);
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-rsync-download-"));
    t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
    const bin = path.join(dir, "bin");
    fs.mkdirSync(bin);
    // Use the system shasum on macOS; CI uses GNU sha256sum.
    if (process.platform === "darwin") {
      fs.writeFileSync(path.join(bin, "sha256sum"), '#!/bin/sh\nexec /usr/bin/shasum -a 256 "$@"\n', { mode: 0o755 });
    }
    fs.mkdirSync(path.join(dir, "rsync-3.4.4"));
    const calls = path.join(dir, "calls.jsonl");
    const payload = "verified fixture bytes\n";
    fs.writeFileSync(path.join(bin, "curl"), `#!${process.execPath}
const fs = require('node:fs');
const args = process.argv.slice(2);
fs.appendFileSync(process.env.FIXTURE_CALLS, JSON.stringify({tool:'curl', args})+'\\n');
const url = args.at(-1);
const sources = ['https://download.samba.org/pub/rsync/src/rsync-3.4.4.tar.gz', 'https://github.com/RsyncProject/rsync/releases/download/v3.4.4/rsync-3.4.4.tar.gz'];
if (!sources.includes(url)) process.exit(99);
const outcome = url === sources[0] ? process.env.FIXTURE_PRIMARY : process.env.FIXTURE_FALLBACK;
const output = args[args.indexOf('--output')+1];
fs.writeFileSync(output, outcome === 'good' ? ${JSON.stringify(payload)} : 'partial or corrupt fixture');
process.exit(outcome === 'partial' ? 28 : 0);
`, { mode: 0o755 });
    const effect = `#!${process.execPath}
const fs = require('node:fs');
const path = require('node:path');
fs.appendFileSync(process.env.FIXTURE_CALLS, JSON.stringify({tool:path.basename(process.argv[1]), args:process.argv.slice(2)})+'\\n');
`;
    for (const file of [path.join(bin, "tar"), path.join(bin, "make"), path.join(dir, "rsync-3.4.4", "configure")]) {
      fs.writeFileSync(file, effect, { mode: 0o755 });
    }
    const result = spawnSync("bash", ["-euo", "pipefail", "-c", script], {
      encoding: "utf8", timeout: 10000,
      env: {
        PATH: `${bin}:${process.env.PATH}`, RUNNER_TEMP: dir,
        GITHUB_PATH: path.join(dir, "github-path"), RSYNC_VERSION: "3.4.4",
        RSYNC_SHA256: createHash("sha256").update(payload).digest("hex"),
        FIXTURE_CALLS: calls, FIXTURE_PRIMARY: scenario.primary,
        FIXTURE_FALLBACK: scenario.fallback ?? "unexpected",
      },
    });
    assert.equal(result.error, undefined);
    assert.equal(result.signal, null);
    assert.equal(result.status === 0, Boolean(scenario.succeeds), result.stdout + result.stderr);
    if (scenario.succeeds) assert.match(result.stdout, /: OK\n/);
    if (scenario.primary === "corrupt" || scenario.fallback === "corrupt") {
      assert.match(result.stdout, /: FAILED\n/);
    }
    const observed = fs.readFileSync(calls, "utf8").trim().split("\n").map(JSON.parse);
    const downloads = observed.filter((call) => call.tool === "curl");
    assert.equal(downloads.length, scenario.calls);
    const urls = ["https://download.samba.org/pub/rsync/src/rsync-3.4.4.tar.gz", "https://github.com/RsyncProject/rsync/releases/download/v3.4.4/rsync-3.4.4.tar.gz"];
    for (let i = 0; i < downloads.length; i++) {
      assert.deepEqual(downloads[i].args, [
        "--disable", "--fail", "--location", "--silent", "--show-error",
        "--proto", "=https", "--proto-redir", "=https", "--connect-timeout", "10", "--max-time", "60",
        "--output", path.join(dir, "rsync-3.4.4.tar.gz"), urls[i],
      ]);
    }
    assert.deepEqual(observed.filter((call) => call.tool !== "curl").map((call) => call.tool),
      scenario.succeeds ? ["tar", "configure", "make", "make"] : []);
    if (scenario.succeeds) assert.equal(fs.readFileSync(path.join(dir, "rsync-3.4.4.tar.gz"), "utf8"), payload);
    else assert.equal(fs.existsSync(path.join(dir, "github-path")), false);
  });
}

// The gate requires the concurrency subtest to run and pass alongside its
// parent, so a successful fixture has to emit both. The negative scenarios keep
// the subtest's own missing and skipped cases covered.
for (const scenario of [
  { name: "executed successfully", actions: ["run", "pass"], concurrency: ["run", "pass"], succeeds: true },
  { name: "skipped native fixture", actions: ["run", "skip"], concurrency: ["run", "skip"] },
  { name: "missing native fixture", actions: [] },
  { name: "Go fails after a passing test event", actions: ["run", "pass"], concurrency: ["run", "pass"], exit: 1 },
  { name: "missing concurrency coverage", actions: ["run", "pass"] },
  { name: "skipped concurrency coverage", actions: ["run", "pass"], concurrency: ["run", "skip"] },
  { name: "concurrency skip after pass", actions: ["run", "pass"], concurrency: ["run", "pass", "skip"] },
]) {
  test(`native lifecycle gate: ${scenario.name}`, (t) => {
    const marker = "      - name: Verify native local-container lifecycle and cleanup\n";
    const script = runScript(workflow.slice(workflow.indexOf(marker) + marker.length));
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-native-lifecycle-gate-"));
    t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
    const bin = path.join(dir, "bin");
    fs.mkdirSync(bin);
    fs.writeFileSync(path.join(bin, "go"), `#!${process.execPath}
process.stdout.write(process.env.NATIVE_RECORDS);
process.exit(Number(process.env.NATIVE_EXIT));
`, { mode: 0o755 });
    const records = [
      ...scenario.actions.map((Action) => ({ Test: "TestLocalContainerProviderE2E", Action })),
      ...(scenario.concurrency ?? []).map((Action) => ({
        Test: "TestLocalContainerProviderE2E/concurrent-cli-warmups",
        Action,
      })),
    ];
    // Match the implicit Actions bash shell; the step must own pipefail.
    const result = spawnSync("bash", ["-e", "-c", script], {
      encoding: "utf8", timeout: 10000,
      env: {
        PATH: `${bin}:${process.env.PATH}`, RUNNER_TEMP: dir,
        NATIVE_RECORDS: records.map((record) => JSON.stringify(record)).join("\n"),
        NATIVE_EXIT: String(scenario.exit ?? 0),
      },
    });
    assert.equal(result.error, undefined);
    assert.equal(result.signal, null);
    assert.equal(result.status === 0, Boolean(scenario.succeeds), result.stdout + result.stderr);
  });
}

test("failed bootstrap diagnostics read only the unique smoke container", (t) => {
  const marker = "      - name: Diagnose local-container bootstrap\n";
  const step = workflow.slice(workflow.indexOf(marker) + marker.length);
  const script = runScript(step);
  assert.match(step, /if: failure\(\) && matrix\.name == 'local-container'/);
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-bootstrap-diagnostics-"));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const docker = path.join(dir, "docker");
  fs.writeFileSync(
    docker,
    `#!/usr/bin/env bash
set -eu
printf '%s\\n' "$*" >> "$DIAGNOSTIC_CALLS"
case "$1" in
  ps) printf '%s\\n' "$DIAGNOSTIC_IDS" ;;
  inspect) printf '{"Running":false,"ExitCode":100}\\n' ;;
  logs) printf 'synthetic apt failure\\n' ;;
  *) exit 99 ;;
esac
`,
    { mode: 0o755 },
  );
  fs.writeFileSync(path.join(dir, "timeout"), '#!/usr/bin/env bash\nshift\nexec "$@"\n', {
    mode: 0o755,
  });
  const log = path.join(dir, "local-container-smoke.log");
  const calls = path.join(dir, "calls.log");
  const lease = "cbx_123456789abc";
  const container = "a".repeat(64);
  const launch = `provisioning provider=local-container lease=${lease} slug=synthetic\n`;
  for (const scenario of [
    { name: "exact", log: launch, ids: container, inspect: true },
    { name: "missing log", log: null, ids: container, inventory: false },
    { name: "no lease", log: "build failed\n", ids: container, inventory: false },
    { name: "multiple leases", log: launch + launch, ids: container, inventory: false },
    { name: "no container", log: launch, ids: "" },
    { name: "ambiguous containers", log: launch, ids: container + "\n" + "b".repeat(64) },
  ]) {
    fs.writeFileSync(calls, "");
    if (scenario.log === null) fs.rmSync(log, { force: true });
    else fs.writeFileSync(log, scenario.log);
    const result = spawnSync("bash", ["-euo", "pipefail", "-c", script], {
      encoding: "utf8",
      env: {
        ...process.env,
        PATH: `${dir}:${process.env.PATH}`,
        RUNNER_TEMP: dir,
        DIAGNOSTIC_CALLS: calls,
        DIAGNOSTIC_IDS: scenario.ids,
      },
    });
    assert.equal(result.status, 0, `${scenario.name}: ${result.stdout}${result.stderr}`);
    const observed = fs.readFileSync(calls, "utf8");
    if (scenario.inventory === false) assert.equal(observed, "", scenario.name);
    else
      assert.match(
        observed,
        new RegExp(
          `^ps -aq --no-trunc --filter label=crabbox=true --filter label=provider=local-container --filter label=lease=${lease}\\n`,
        ),
      );
    if (scenario.inspect) {
      assert.match(
        observed,
        new RegExp(`inspect --format \\{\\{json \\.State\\}\\} ${container}\\n`),
      );
      assert.match(observed, new RegExp(`logs --tail 100 ${container}\\n`));
      assert.match(result.stdout, /synthetic apt failure/);
    } else assert.doesNotMatch(observed, /^(inspect|logs) /m, scenario.name);
    assert.doesNotMatch(observed, /Config|(^|\n)(rm|exec|stop|kill) /);
  }
});
