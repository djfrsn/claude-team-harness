import { spawnSync } from "node:child_process";

const failures = [
  {
    command: "tsc",
    args: [
      "--noEmit",
      "--project",
      "fixtures/quality-gate/tsconfig.unsafe.json",
    ],
    expects: "error TS7006",
    name: "unsafe type",
  },
  {
    command: "oxlint",
    args: [
      "--config",
      ".oxlintrc.json",
      "fixtures/quality-gate/cycle-a.ts",
      "fixtures/quality-gate/cycle-b.ts",
    ],
    expects: "import(no-cycle)",
    name: "dependency cycle",
  },
  {
    command: "vitest",
    args: [
      "run",
      "--coverage",
      "--config",
      "fixtures/quality-gate/vitest.config.ts",
    ],
    expects: "does not meet global threshold",
    name: "coverage drop",
  },
];

for (const fixture of failures) {
  const result = spawnSync(fixture.command, fixture.args, {
    encoding: "utf8",
    stdio: "pipe",
  });
  if (result.error !== undefined) {
    throw result.error;
  }
  if (result.status === 0) {
    throw new Error(`quality fixture did not reject: ${fixture.name}`);
  }
  const output = `${result.stdout}${result.stderr}`;
  if (!output.includes(fixture.expects)) {
    throw new Error(
      `quality fixture failed for the wrong reason: ${fixture.name}`,
    );
  }
  console.log(`quality fixture rejected: ${fixture.name}`);
}
