import { build } from "esbuild";
import { copyFile, rm } from "node:fs/promises";

await rm("dist", { force: true, recursive: true });
await build({
  bundle: true,
  entryPoints: ["src/main.tsx"],
  format: "esm",
  jsx: "automatic",
  jsxImportSource: "preact",
  legalComments: "none",
  minify: true,
  outdir: "dist",
  target: ["es2022"],
});
await copyFile("index.html", "dist/index.html");
