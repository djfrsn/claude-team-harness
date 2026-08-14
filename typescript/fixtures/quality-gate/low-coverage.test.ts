import { expect, test } from "vitest";

import { outcome } from "./low-coverage.ts";

test("covers one outcome", () => {
  expect(outcome(true)).toBe("success");
});
