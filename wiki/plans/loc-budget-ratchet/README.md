# LOC Budget Ratchet

## Change

Turn the fixed source line ceiling into a 100-line ratchet. Normal changes use
the current headroom. A change that crosses a band records the smallest required
increase in the same pull request.

## Requirements

- As a contributor, when my change fits the current ceiling, I can pass the
  gate without changing the budget.
- As a contributor, when my change crosses the ceiling, I receive a command
  that raises it to the smallest valid 100-line band.
- As a reviewer, when a pull request raises the ceiling, CI proves that the
  merged change needs the raise and that the new ceiling is the smallest band.
- As a contributor, when the repository shrinks, I can tighten the ceiling to
  the smallest band above the new count.
- As a contributor, tracked and non-ignored untracked text files use the same
  count during local work and CI.

## Design

`scripts/gate/loc.sh` owns counting, band calculation, checks, raises, and
tightening. The budget remains one integer in `loc-budget.txt`. Pull-request CI
fetches the base branch and compares its ceiling with the proposed ceiling.
Increases require a count above the base ceiling and must equal the first band
strictly above the current count. The harness continues to count `wiki/`.

## Test

A shell fixture creates a temporary Git repository. It verifies an unchanged
check, untracked-file counting, an over-budget failure, the smallest raise, a
valid base-aware raise, rejected premature and oversized raises, tightening,
and invalid budget content. The full quality gate runs the fixture.
