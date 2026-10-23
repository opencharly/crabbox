import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { spawnSync } from "node:child_process";
import test from "node:test";
import { writeExecutable } from "./test-support/smoke-fixtures.mjs";

const repoRoot = path.resolve(import.meta.dirname, "..");

function moduleFixture(t, findScript) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-test-go-modules-"));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const root = path.join(dir, "repo with spaces");
  const bin = path.join(dir, "bin");
  const goLog = path.join(dir, "go.log");
  fs.mkdirSync(bin);
  fs.mkdirSync(path.join(root, "scripts"), { recursive: true });
  fs.copyFileSync(
    path.join(repoRoot, "scripts/test-go-modules.sh"),
    path.join(root, "scripts/test-go-modules.sh"),
  );
  const modules = [".", "nested module", "other/deep module"];
  for (const module of modules) {
    fs.mkdirSync(path.join(root, module), { recursive: true });
    fs.writeFileSync(path.join(root, module, "go.mod"), "module example.test/fixture\n");
  }
  if (findScript) {
    writeExecutable(path.join(bin, "find"), `#!/usr/bin/env bash\nset -euo pipefail\n${findScript}`);
  }

  writeExecutable(
    path.join(bin, "go"),
    `#!/usr/bin/env bash
set -euo pipefail
if [ "\${1:-}" = "list" ]; then
  printf '%s\\n' example.test/fixture example.test/fixture/internal/cli example.test/fixture/other
  exit "\${GO_LIST_EXIT:-0}"
fi
printf '%s\\t%s\\n' "$PWD" "$*" >>"$GO_TEST_LOG"
exit "\${GO_TEST_EXIT:-0}"
`,
  );

  return {
    root,
    modules,
    calls: () => (fs.existsSync(goLog) ? fs.readFileSync(goLog, "utf8").trimEnd().split("\n") : []),
    run: (exitCode = 0, extraEnv = {}) => {
      const result = spawnSync("bash", ["scripts/test-go-modules.sh"], {
        cwd: root,
        env: {
          ...process.env,
          PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
          GO_TEST_LOG: goLog,
          GO_TEST_EXIT: String(exitCode),
          ...extraEnv,
        },
        encoding: "utf8",
      });
      assert.ifError(result.error);
      return result;
    },
  };
}

for (const discovery of ["real find", "fake find"]) {
  test(`all modules including root run once with a 20m timeout and spaced paths (${discovery})`, (t) => {
    const fixture = moduleFixture(
      t,
      discovery === "fake find"
        ? `printf '%s\\0' "$1/go.mod" "$1/nested module/go.mod" "$1/other/deep module/go.mod"\n`
        : undefined,
    );
    for (const excluded of [".git", "node_modules", "dist", "dist-cloudflare"]) {
      fs.mkdirSync(path.join(fixture.root, excluded));
      fs.writeFileSync(path.join(fixture.root, excluded, "go.mod"), "module example.test/excluded\n");
    }
    const result = fixture.run();
    assert.equal(result.status, 0, result.stdout + result.stderr);
    assert.deepEqual(
      fixture.calls().sort(),
      fixture.modules
        .map((module) => {
          const dir = fs.realpathSync(path.join(fixture.root, module));
          // The root module runs the packages go list resolves, minus its
          // internal/cli package; every other module keeps ./...
          const target = module === "." ? "example.test/fixture example.test/fixture/other" : "./...";
          return `${dir}\ttest -timeout=20m ${target}`;
        })
        .sort(),
    );
    assert.match(result.stdout, /go test -timeout=20m \.\/\.\.\./);
  });
}

test("go module discovery failure aborts before reporting success", (t) => {
  const fixture = moduleFixture(
    t,
    `printf '%s\\0' "$1/go.mod"
printf 'find: partial traversal failure\\n' >&2
exit 7
`,
  );
  const result = fixture.run();

  assert.notEqual(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stderr, /failed to discover go\.mod files/);
  assert.deepEqual(fixture.calls(), [], "go test should not run after incomplete discovery");
});

test("empty go module discovery fails closed", (t) => {
  const fixture = moduleFixture(t, "exit 0\n");
  const result = fixture.run();
  assert.notEqual(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stderr, /no go\.mod files found/);
  assert.deepEqual(fixture.calls(), []);
});

test("ordinary module test failure propagates without running later modules", (t) => {
  const fixture = moduleFixture(
    t,
    `printf '%s\\0' "$1/go.mod" "$1/nested module/go.mod"\n`,
  );
  const result = fixture.run(23);
  assert.equal(result.status, 23, result.stdout + result.stderr);
  assert.deepEqual(fixture.calls(), [`${fs.realpathSync(fixture.root)}\ttest -timeout=20m example.test/fixture example.test/fixture/other`]);
});

test("a failing go list fails closed instead of testing a partial package list", (t) => {
  const fixture = moduleFixture(
    t,
    `printf '%s\\0' "$1/go.mod"`,
  );
  // The fake go list emits packages and then exits nonzero, as Go does when one
  // package cannot load. Testing only the emitted subset would hide that package.
  const result = fixture.run(0, { GO_LIST_EXIT: "7" });

  assert.notEqual(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stderr, /go list failed for the root module/);
  assert.deepEqual(fixture.calls(), [], "go test must not run against a partial package list");
});
