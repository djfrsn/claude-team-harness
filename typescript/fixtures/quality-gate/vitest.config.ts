import { defineConfig, mergeConfig } from "vitest/config";

import production from "../../vitest.config.ts";

export default mergeConfig(
  production,
  defineConfig({
    test: {
      coverage: {
        exclude: ["fixtures/quality-gate/low-coverage.test.ts"],
        include: ["fixtures/quality-gate/low-coverage.ts"],
      },
      include: ["fixtures/quality-gate/low-coverage.test.ts"],
    },
  }),
);
