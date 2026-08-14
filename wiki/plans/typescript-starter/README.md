# TypeScript Starter

## Change

Add Studio's strict TypeScript quality floor and frontend stack as a standalone
hello-world package. The package supplies a runnable Preact surface without
bringing Studio console behavior or repository deployment automation.

## Requirements

- As a contributor, when I clone the repository on another computer, I can
  install the frozen TypeScript dependencies and run one complete check.
- As a contributor, when I add unsafe types, a dependency cycle, an unhandled
  promise, unused code, bad formatting, or insufficient tests, the TypeScript
  gate fails.
- As a reviewer, when the quality configuration weakens, executable fixtures
  prove that unsafe types, cycles, and low coverage still fail.
- As a developer, when I build the sample, I receive a static Preact app that
  renders the Claude Team Harness hello-world screen.
- As a user, when I view the sample on a small screen or request reduced motion,
  the interface remains readable and respects that preference.

## Design

The `typescript/` directory is a private pnpm package pinned to Node 24 and
pnpm 11.19. It uses Preact, TypeScript, esbuild, Vitest, V8 coverage, Oxlint,
Biome, and Knip at the exact versions established in Studio. The package keeps
its compiler, lint, format, dependency, test, coverage, negative-fixture, and
build commands behind `pnpm check`.

The shared gate starts the package check when the package exists. Hosted CI
installs the pinned runtime and frozen dependencies first. Generated pnpm lock
content stays outside the source LOC count; the configuration, app, and tests
remain inside the ratchet.

## Test

Run a frozen install in an empty dependency directory, then run `pnpm check`.
Confirm the strict compiler, both lint modes, dependency scan, format check,
component test, coverage thresholds, three negative fixtures, and production
bundle pass. Run the complete repository gate and its pull-request base-aware
LOC check. Inspect the built app at desktop and narrow viewport sizes.
