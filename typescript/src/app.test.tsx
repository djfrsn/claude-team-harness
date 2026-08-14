import { h } from "preact";
import renderToString from "preact-render-to-string";
import { expect, test } from "vitest";

import { App } from "./app.tsx";

test("renders the ready starter and its quality floor", () => {
  const output = renderToString(h(App, null));

  expect(output).toContain("Hello,");
  expect(output).toContain("Starter online");
  expect(output).toContain("4 checks armed");
  expect(output).toContain("Strict types");
  expect(output).toContain("Dependency hygiene");
  expect(output).toContain("pnpm check");
});
