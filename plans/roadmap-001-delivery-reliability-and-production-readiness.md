# Delivery reliability and production readiness

## Source

- User requested implementation of all six improvements from the November 3, 2026 conversation, with a separate PR merged into main for each, and explicitly invoked tmux-act for the full lane.
- Prior delivery recovery work: issue #228 and merged PR #229.
- Current implementation, DESIGN.md, README.md, docs/, and .github/workflows/.
- Related open work: #223 production operations, #128 runnable HTMX example, #187 existing tracker. Reconcile those issues only when their actual acceptance criteria are met.

## Goals

Make subscription behavior consistent, failures observable, HTTP streaming operable, public APIs understandable, releases reproducible, and deployment behavior demonstrable.

## Non-Goals

Distributed presence, new tenant policy engines, application state storage, keyed coalescing, browser acknowledgments or exactly-once promises, replacement of SSE, and unrelated feature expansion.

## Execution and Acceptance

The user authorized continuous roadmap-run execution. Build only the next bounded packet from the current code. Each numbered pass has its own feature branch and PR targeting main. Codex reviews the implementation, routes bounded corrections through the same pass, verifies required CI, and merges that PR before starting the next pass. Never bypass required checks or force push. Use explicit squash messages without AI attribution. Include this roadmap in the first pass PR; executable packets remain ignored local files.

The existing commit_policy: ask is satisfied by the user's explicit commit/PR/merge instruction. Required executor cleanup remains in effect. A trusted clean review can advance this lane without another permission question. Mark a pass complete only after review and merge; keep PR links in its Review field. Reassess candidate work from evidence at each pass rather than imposing speculative designs.

## Passes

- [x] Pass 001: Consistent subscription lifecycle.
  - Requirements: Shared internal lifecycle ownership where it prevents divergence; safe registration, unsubscribe, administrative disconnect, eviction, broker shutdown, backend first/last transitions, and worker cancellation across normal, scoped, filtered, snapshot, replay, rate and coalescing subscriptions. Fix demonstrated shutdown/send races. Preserve public signatures and documented delivery differences. Establish reusable lifecycle contract tests.
  - Plan: .agents/.local/plans/handoff-005-consistent-subscription-lifecycle.md
  - Review: https://github.com/catgoose/tavern/pull/230 (merged).
    - Reviewed commit: `9d00ba25172980e84f7a10fe7338bd87b4142416`

- [x] Pass 002: Observable subscription and storage failures.
  - Requirements: Applications can observe useful structured termination reasons without depending on a full message queue. Replay-store errors become explicit, observable failures with defined recovery behavior rather than looking like successful empty replay. Metrics/docs distinguish queued delivery from transport writes and application processing. Prefer additive compatibility and bounded observability state.
  - Plan: .agents/.local/plans/handoff-006-observable-subscription-and-replay-storage-failures.md
  - Review: https://github.com/catgoose/tavern/pull/231 (merged).
    - Reviewed commit: `22398ab2385479bc51db29ef5314a87ed68c3b0e`

- [x] Pass 003: HTTP streaming deadlines and production operations.
  - Requirements: Configurable per-write deadlines, observable write/flush failures, and predictable cancellation for supported standard HTTP writers and wrappers. Document unsupported capabilities and keep custom writers usable. Complete an operator guide covering proxy buffering, heartbeats, replay retention, reconnect bursts, slow clients, monitoring, and graceful shutdown. Validate issue #223 against the result.
  - Plan: .agents/.local/plans/handoff-008-http-streaming-deadlines-and-production-operations.md
  - Review: https://github.com/catgoose/tavern/pull/232 (merged).
    - Reviewed commit: `c77f4c1e0edf9fd2b8a18e546513272fa44bf2b6`

- [x] Pass 004: Public API guidance and option validation.
  - Requirements: Document recommended latest-representation, invalidation/reconciliation, and ID/replay patterns with a verified subscription compatibility table. Provide an additive way to validate configurations and handle invalid combinations without runtime surprises. Converge implementation differences where compatible, preserve existing contracts, and explicitly explain remaining replay/snapshot/filter differences.
  - Plan: .agents/.local/plans/handoff-010-subscription-api-guidance-and-option-validation.md
  - Review: https://github.com/catgoose/tavern/pull/233 (merged).
    - Reviewed commit: `e78acc040dc194707519a5469730accaaff387db`

- [x] Pass 005: Reproducible CI, dependency updates, and releases.
  - Requirements: Dependency and Go baseline changes are reviewed and tested before reaching main. Exercise root and adapter modules against the local source; pin tooling and make supported Go versions explicit. Separate dependency maintenance from release decisions and keep release checks tied to the exact tested revision. Preserve practical release automation without silently raising consumer requirements or publishing unvalidated code.
  - Plan: .agents/.local/plans/handoff-011-reproducible-ci-and-reviewed-dependency-releases.md
  - Review: https://github.com/catgoose/tavern/pull/234 (merged).
    - Reviewed commit: `427ae22cb21baf4c5836e319a0ab53254eb7c4f0`

- [x] Pass 006: Reference deployment and capacity validation.
  - Requirements: A runnable server-owned UI reference demonstrates snapshot, updates, distinct invalidations, loss/reconnect, canonical refetch, and shutdown. Test real HTTP streaming and a reproducible proxy deployment. Supply a repeatable load/capacity exercise covering fanout, payload sizes, retained bytes, slow consumers and reconnect bursts, with measured limits and assumptions instead of universal capacity claims. Reconcile #128 and #187 with delivered evidence; the user's current authorization includes building this example.
  - Plan: .agents/.local/plans/handoff-012-reference-deployment-and-repeatable-capacity-validation.md
  - Review: https://github.com/catgoose/tavern/pull/236 (merged).
    - Reviewed commit: `90c7079bec65561ed74db41a8a11cc39f33258f7`

## Review Risks

- Lifecycle refactors can silently change replay ordering, snapshot placement, filter behavior, hooks, scope isolation, and partial aggregate continuation.
- Backend transitions can race with registration or teardown; tests must check delivery and resource release, not just callback counts.
- New failure reporting must avoid unbounded per-subscriber state, synchronous slow callbacks, or a false browser-delivery claim.
- Write deadlines and custom HTTP wrappers differ in capability; context cancellation alone does not interrupt every blocked writer.
- Changing errors or panic behavior can break callers. Prefer additive APIs and explicit migration notes.
- Root-module tests do not automatically cover nested adapter modules. Release and dependency changes must not weaken coverage.
- Capacity numbers depend on workload and environment; retain the recipe and assumptions with any measurements.

## Deferred Questions

- Exact termination/validation API shapes are decided in the relevant packet from current code, favoring additive compatibility.
- Additional backend implementations, hard byte-budget APIs, and distributed delivery-gap propagation remain candidates if the capacity/deployment work demonstrates a concrete need; they are not implicit additions to this lane.
- Preserve the current declared Go minimum initially; pass 005 establishes a documented baseline from verified code/dependency requirements instead of guessing an older supported version.

## Status

All six passes are reviewed and merged into main. No further passes are scheduled in this lane. Issue #128 retains the broader example scope listed in PR #236; distributed presence and tenant controls in #187 remain outside this roadmap.
