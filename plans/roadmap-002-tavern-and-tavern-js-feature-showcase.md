# Tavern and tavern-js feature showcase

## Source

- https://github.com/catgoose/tavern/issues/128: runnable HTMX application demonstrating Tavern's features.
- November 4, 2026 conversation: demonstrate all current Tavern features, include tavern-js, and use the application to discover and address integration gaps. The user authorized creating tavern-js issues and separate plans for needed tavern-js work, coordinated from this workspace, and requested tmux-act after writing this roadmap.
- Existing reference: examples/taskboard/, docs/subscription-guide.md, docs/stream-contract.md, docs/multi-user-coordination.md, README.md, RECIPES.md and the public Go APIs.
- Companion: https://github.com/catgoose/tavern-js, initially v0.0.23 at commit f3166d8e781b250cf604e0f5ed44c306284d33ff. Its README and implementation define the client feature inventory.
- https://github.com/catgoose/tavern-js/issues/50: live form interaction insulation.
- https://github.com/catgoose/tavern-js/issues/53: explicit reconciliation after suppressed updates when no further SSE event arrives; opened from a source-level/jsdom reproduction while preparing this roadmap.
- Previous roadmap plans/roadmap-001-delivery-reliability-and-production-readiness.md is complete. This is a new lane expanding its reference application.

## Goals

- Deliver one coherent, runnable showcase for Tavern and tavern-js: a useful collaborative taskboard plus focused interactive scenarios for features that need independent settings or workloads.
- Account for every current public capability in a maintained coverage matrix: scenario or runnable server exercise, source location, visible behavior, automated evidence, and delivery status. Group equivalent API variants while naming what they cover; an inventory entry or a link to a unit test alone is not a finished demonstration.
- Make connection state, recovery, stream transitions, interaction safety, routing, shaping, hooks, and operations observable through real library behavior.
- Keep the normal launch to go run ./examples/taskboard with local, pinned browser assets, minimal CSS, and no frontend build or CDN requirement. Browser test tooling may have separate developer dependencies.
- Test the real Go/HTMX/SSE/tavern-js integration in a browser from the first pass, expanding coverage alongside implementation.
- Use observed integration problems to drive bounded fixes and issues in the owning library, with reviewed versions pinned into the demo.

## Non-Goals

- A new frontend framework, client-owned application state, production identity service, or persistent application database.
- Implementing distributed presence (#151) or tenant policy engines (#154), adding new production backend implementations, or promising durable/exactly-once browser delivery.
- A separate screen for every exported symbol or Go configuration setter. Equivalent variants can share a clear scenario; headless server capabilities need a runnable exercise with observable results.
- Mandatory speculative tavern-js features. Existing hooks or application recipes may satisfy the behavior; make additive library changes only when evidence supports them.

## Execution and Acceptance

- Build only the next bounded packet from current source and the next unchecked pass. Future passes describe intent and can be split when evidence shows that a separate PR is needed.
- Pass 001 ran in standalone mode. On November 4, after reviewing its completion and remaining passes, the user requested continuing the roadmap. From pass 002, use roadmap-run: review and merge each bounded pass, then build the next packet from current source. Routine corrections remain within their pass.
- Retain the user's established feature-branch, explicit-commit, separate-PR, review, green-CI and squash-merge-to-main policy. The commit_policy: ask setting is satisfied by that authorization. Claude leaves implementation PRs open for Codex review; never bypass checks or force push. No AI attribution.
- Include this roadmap in the first implementation PR; executable packets remain ignored under .agents/.local/plans/. Mark passes complete only after review and merge, with PR links in their Review fields.
- Create tavern-js issues for concrete demonstrated gaps, checking existing issues first. Each issue needs a reproduction or concrete missing behavior, acceptance criteria, and a link to the relevant showcase scenario. Reuse #50 and #53 rather than duplicating them.
- Needed tavern-js changes may be planned here, but execute in a separate bounded packet against the tavern-js repository, with that repository's review identity, tests, branch and PR. Preserve unrelated local files. Review and merge the upstream change before updating the demo's pinned asset. Do not overwrite an unreviewed execution or hide cross-repository changes inside a Tavern-only review.
- Keep #128 open until the full coverage matrix and browser evidence substantiate completion; reconcile #187 only for the delivered scope. Do not close an upstream issue merely because a demo workaround exists.

## Passes

- [x] Pass 001: Showcase foundation and browser integration.
  - Requirements: Evolve the existing taskboard with a navigable showcase entry point, an honest feature coverage matrix for both libraries, locally pinned tavern-js with provenance/license, and a small working connection/update scenario using the real server and client. Establish reproducible browser testing in local commands and shared CI. Preserve the existing taskboard, loss-recovery demonstration and proxy validation. Navigation exposes implemented scenarios; planned coverage stays in documentation.
  - Plan: .agents/.local/plans/handoff-016-showcase-foundation-and-real-browser-integration.md
  - Review: https://github.com/catgoose/tavern/pull/239 (merged); reviewed head 099c04bbac255655893ca7aafa903b997fa2f4cb, merge commit 09e8e3eeb5fed82745ac3ae6f11cc0f95dd62ae2.

- [ ] Pass 002: Collaborative server-rendered taskboard.
  - Requirements: Demonstrate task CRUD, actual OOB replace/append/delete, snapshot plus live updates, lazy/component rendering where supported, and clearly distinguished invalidation/refetch behavior. Exercise scoped per-viewer delivery, groups and subscriber filters with two browser identities. Demonstrate bulk publishing without implying application transactions or atomic cross-region rendering. Keep application authorization separate from routing.
  - Plan: .agents/.local/plans/handoff-017-collaborative-taskboard-with-scoped-oob-updates.md
  - Review: pending.

- [ ] Pass 003: Safe interaction and editing.
  - Requirements: Exercise programmatic and delegated commands, pointer capture/deduplication and keyboard access, hearth shorthand, and overlapping pointer/focus protection under DOM churn. Preserve active drafts and make conflicts visible using server-owned state. Prove reconciliation after a final suppressed update without another publish. Evaluate tavern-js #50 and #53 through concrete browser scenarios; create separate upstream packets for any necessary fixes, then consume reviewed pinned assets.
  - Plan: pending.
  - Review: pending.

- [ ] Pass 004: Connection lifecycle and stream navigation.
  - Requirements: Demonstrate a stable app-shell lifeline and scoped streams that warm, promote, retire and fall back on navigation or failure; dynamic DOM binding and teardown; multi-topic/glob subscriptions and server-initiated topic changes. Surface transport, recovering, stale/live, region-updated and degradation events according to their actual meanings. Verify repeated navigation releases subscriptions and listeners without losing the lifeline.
  - Plan: pending.
  - Review: pending.

- [ ] Pass 005: Replay, loss and recovery.
  - Requirements: Provide deterministic controls for ID-backed replay, Last-Event-ID resume, gaps, fallback snapshots, truncation, reconnect callbacks/bundling, bounded replay retention and replay-store failures. Show gap action variants and client recovery feedback, loss-aware invalidations, queue overflow and eviction, including status/termination reasons. Prove eventual canonical state after failures; distinguish enqueued replay from confirmed browser application.
  - Plan: pending.
  - Review: pending.

- [ ] Pass 006: Delivery shaping and reactive workflows.
  - Requirements: Build focused, observable scenarios for publish-if-changed, debounce, throttle, TTL/OOB expiry, batching, ordering/deduplication, subscriber rate limiting, latest-value coalescing and adaptive pressure. Include After dependencies, mutation hooks, publish middleware, scheduled publishers, render failures and circuit breakers. Compare produced versus observed messages with bounded state and deterministic controls, using only compatible subscription options and accurate TTL/batch/ordering semantics.
  - Plan: pending.
  - Review: pending.

- [ ] Pass 007: Presence and operational diagnostics.
  - Requirements: Demonstrate structured presence and connection events with explicit per-connection/per-user/process boundaries, admission limits, write/flush failures, heartbeats, maximum connection lifetime and graceful shutdown. Expose useful broker/per-topic metrics and observability. Supply runnable opt-in exercises for memory backend fanout, replay storage and Prometheus/OpenTelemetry adapters without adding adapter dependencies to core. Reuse existing proxy/capacity tooling where it covers a capability and explain what it proves.
  - Plan: pending.
  - Review: pending.

- [ ] Pass 008: Complete coverage and evaluation experience.
  - Requirements: Resolve every uncovered matrix entry through a working scenario or runnable exercise, with explicit treatment of aliases and test-helper APIs. Complete browser coverage of two-user updates, scope isolation, idle-after-edit convergence, hot commands, navigation/teardown, reconnect/gaps and pressure recovery. Check keyboard/status accessibility, source links, startup/reset instructions, pinned asset reproduction and bounded resource cleanup. Reconcile #128 against the original criteria and verified full-library coverage; accurately close or retain upstream issues.
  - Plan: pending.
  - Review: pending.

## Review Risks

- HTMX/SSE lifecycle timing differs from jsdom; a server-only test or manually dispatched browser event cannot stand in for real transport/DOM integration.
- The existing taskboard uses custom ready/invalidation frames. Adding tavern-js must respect the control-event contract and canonical fetch completion rather than equating an opened socket with successful recovery.
- Interaction suppression drops intermediate events. Arbitrary append/delete deltas cannot be safely treated as replaceable snapshots; finite streams need reconciliation even when the server becomes idle.
- Scope isolation, lifeline ownership and dynamic navigation can leak connections/listeners or deliver another viewer's data. Demo identity controls are not production authorization.
- Batch publication is not a database transaction or guaranteed atomic browser rendering. TTL and ordering guarantees must match the implemented API.
- Failure controls need deterministic bounded ownership; avoid global sleeps, unbounded logs, shared cross-scenario pressure or cleanup of resources the demo did not create.
- Browser assets must be pinned with provenance/licenses, and browser CI must participate in the existing required aggregate check. Optional adapters must not raise core dependencies.
- Separate repositories have separate worktrees, CI and review baselines; consume only reviewed upstream revisions and preserve existing local changes.

## Deferred Questions

- The initial public-capability inventory determines the exact scenario grouping; group API aliases without silently omitting capabilities. New APIs added during the lane must be considered before completion.
- Resolve the precise live-form insulation and post-suppression recipe from #50/#53 evidence. Prefer the smallest server-authoritative solution; an issue does not mandate a particular new API.
- Choose the smallest maintained browser harness that runs locally and in CI with pinned developer dependencies. There is a local Google Chrome binary; previous lack of browser evidence is not a reason to omit browser validation.
- Headless extension points, helper APIs and optional exporters may be demonstrated through runnable server exercises linked from the showcase rather than misleading browser controls.
- Release/tag availability can affect consumption timing of upstream fixes; pin an exact reviewed commit with provenance if a released asset is not yet available, preserving the no-build launch.

## Suggested Next Plan

- Pass 002: the collaborative taskboard, scoped and filtered views, grouped delivery, real OOB fragments, and snapshot/update consistency. Dispatch handoff-017 in roadmap-run mode. Leave later passes unplanned until their turn.
