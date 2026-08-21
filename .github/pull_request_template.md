## What this changes

<!-- One or two sentences. -->

## Why

<!-- What was wrong, or what could not be done before. -->

## How it was verified

<!--
"Tested against a live Nomad 2.0.4 dev agent" is worth more than a paragraph of
description. Say what you actually ran.
-->

- [ ] `make check` passes
- [ ] `make test-e2e` passes (if this touches anything that talks to Nomad)

## Checklist

- [ ] New tools are registered in `Catalog()` and carry a `ReadOnlyTool()` or
      `MutatingTool()` annotation
- [ ] Mutating tools are added to `expectedMutatingTools` **and**
      `expectedHints` in `pkg/tools/tools_test.go`
- [ ] Tool descriptions say *when* to use the tool and name the likely next one
- [ ] No new Nomad API behaviour was assumed — it was checked against the docs,
      the `nomad/api` source, or a live agent
- [ ] `CHANGELOG.md` updated if behaviour changed
- [ ] Any decision someone might question is explained in the PR description
- [ ] No tokens, real cluster addresses or real job specs anywhere in the diff
