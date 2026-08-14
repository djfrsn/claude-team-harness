const requiredNodeMajor = "24";
const requiredPnpm = "11.19.0";
const nodeMajor = process.versions.node.split(".")[0];
const pnpm = process.env["npm_config_user_agent"]?.match(/^pnpm\/([^ ]+)/)?.[1];

if (nodeMajor !== requiredNodeMajor) {
  throw new Error(
    `Claude Team Harness TypeScript requires Node ${requiredNodeMajor}; found ${process.versions.node}`,
  );
}
if (pnpm !== requiredPnpm) {
  throw new Error(
    `Claude Team Harness TypeScript requires pnpm ${requiredPnpm}; found ${pnpm ?? "unknown"}`,
  );
}
