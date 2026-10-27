# Building Nano

This page governs all work on Nano. Where another document disagrees with it,
this page wins. What Nano is, and why, opens the [README](../README.md).

## The top rule

**Nano is open source, and its code must be impressive.** "Impressive" means
two things:

- It is fit to publish and to recommend: an honest README, a quick start that
  works on a clean machine, disclosed behavior, real tests on the paths that
  matter, and nothing private or left over in the tree.
- It follows common-sense elite software engineering.

It must be robust but not overkill. Too little polish and too much ceremony
are both failures. The judge is a fresh frontier model, from a different
provider than the one that built the work, reading the repo as a skeptical
stranger.

## How it is checked

- **Every candidate.** One fresh reviewer, from a different provider than the
  builder, judges how the candidate fits the whole of Nano against this page.
  It is the candidate's only code review. The reviewer asks whether the
  change adds a copy of a fact, adds always-on work, does work for things not
  on screen, or belongs somewhere else, and whether a simpler way exists. At
  a minimum, it checks that:
  1. No person names, home paths, machine names or private system names
     appear in product code, comments, tests, or fixture names.
  2. No dead code, and no test-only code, sits in production files.
  3. The change reuses existing paths. A second copy of a fact states which
     copy wins.
  4. No always-on work is added (polling, per-frame encoding, whole-store
     scans) without a measured budget.
  5. Tests check behavior where a user or agent sees it, and fixtures are
     small and synthetic or scrubbed. Each behavior has one primary test; the
     reviewer does not ask for the same case at a second boundary.
  6. The core checks pass on the exact tree.
  7. It names what can be removed. For a design, per mechanism: the
     requirement it serves, the simplest alternative, and the difference the
     user would see. A finding that adds a mechanism without naming a
     user-visible failure is advisory.
- **Always.** A light monitor watches Nano's memory and CPU, the provider
  processes, swap, stuck prompts, and background jobs that keep failing,
  each against a target. Performance is monitored, not gated: a performance
  check never blocks a change from going live.
- **Periodically.** A repo-wide review reads the whole repo as a stranger
  would. It does not run on every push.

## Guiding principles

These guide judgment. They are not hard rules to apply blindly, and none of
them is a count.

1. **Less is often more.** Before writing code, ask in order:
   1. Does this need to exist at all?
   2. Does Nano already have it? Then reuse it.
   3. Does the standard library, the platform, or an installed dependency
      cover it?
   4. Only then, write the smallest code that works.

   When code is needed, choose the elegant version: fewer files and fewer
   concepts, deletion over addition, boring over clever. Understand the
   problem fully first, and fix root causes once rather than patching each
   symptom. Never cut validation at trust boundaries, error handling that
   prevents data loss, or security.

2. **Efficient and performant.**
   - The server uses little CPU and memory on the host machine, both idle and
     under load.
   - The browser stays light too, even with many threads and long transcripts
     open.
   - Everything the user touches responds quickly.
   - Nano prepares only what is on screen or in use. At startup it loads only
     the threads visible in the sidebar. A thread that becomes visible, or
     starts working, is prepared then. Idle threads are let go after a set
     time (3 days), and they come back when opened or prompted.

## Lessons from the past

These come from a month of Nano failures.

- **Prefer one source of truth.** When the same fact lives in two places, ask
  whether the second copy is needed. If it is, be clear which copy wins and
  how the two stay in step. *Why:* duplicate messages, a wrong "working"
  status, and lost prompts all came from several copies of one fact. Each fix
  joined only one pair of copies.
- **Protect the user's work.** Treat anything that could lose, duplicate, or
  hide a prompt or message as the most serious kind of bug. Design restarts
  so the user barely notices them. *Why:* every restart was an outage, and
  prompts went missing across restarts.

## Tests

- **The core checks.** About 8 to 12 checks prove Nano still works. Each is
  tied to a feature the README names or to a failure from Nano's history,
  such as lost or duplicated prompts, restarts that break threads, and
  messages shown twice. Only the core checks can block a change from going
  live. The maintainer approves each addition, and a new check replaces any
  it overlaps. Until they exist, the full test suite stands in for them.
- **Other tests don't block.** A failing one has either found a real bug,
  which gets fixed, or it is useless, which means it gets deleted.
- **A failing blocking test is fixed or skipped the same day.** A test is
  flaky only when the same code and inputs both pass and fail; a
  deterministic hang is a bug. After at most an hour on a clear cause, skip
  that exact test unconditionally at its start (`t.Skip`, `it.skip`), naming
  the reason, and keep its name. Never hide a skip in a build tag,
  environment check, retry loop or suite filter, and never delete a test
  only because it is inconvenient. The skip comes out after the cause is
  fixed and the test passes in the ordinary suite; the candidate's reviewer
  verifies the removal.
- **Browser and recovery work starts from a table.** Before the first red
  test, one page of views × events (what each view shows after each event)
  is written and reviewed. Its rows name the tests.
- **Few tests, on the paths that matter.** Thousands of tests are not a
  goal. A test earns its place by protecting something a user or agent
  relies on.

## Who decides

The maintainer decides what Nano does for its user: product behavior,
changes to stored data, and anything destructive. The maintainer approves
what tests must prove, not how a test is run, timed, sized or scoped; those
are the owner's and the reviewer's call.

## Where work lives

`docs/` holds only public, lasting documentation: the architecture, the
decision records under `docs/decisions/`, and feature contracts. Plans,
research and working notes are not part of the repository.
