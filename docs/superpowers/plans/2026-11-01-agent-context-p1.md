# Agent Context P1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement task-by-task. Steps use checkbox syntax for tracking.

**Goal:** Resolve the specialist review and ship the read-only multi-agent context example without changing existing case APIs.

**Architecture:** Preserve delegation, pipeline and parallel; default to role-projected briefs and host-validated reports/artifacts. The host owns versioned invocation/acceptance state and a shared transport limiter, while the existing Builder exclusively reserves/account budgets. Fork and shared-file remain separately gated phases.

**Tech Stack:** Go 1.25, existing pinned GoAgent, Builder, RunnableToolFactory, Adapt, AddParallel, core/budget, existing livemodel.

**Spec:** docs/research/2026-11-01-multi-agent-context.md; review docs/research/2026-11-01-multi-agent-context-review.md.

## Global Constraints

- No v1/v2 API paths. Existing examples stay source-compatible and retain their assertions.
- No speculative core changes: use Builder reservation -> shared host transport permit -> actual request; no double Reserve.
- Provider generation recovery may retry the exact Gemini MALFORMED_FUNCTION_CALL once per child invocation before emitted output, through Builder; no retry of host validation/tool effects and no hidden client retry. All attempts consume shared budget/usage.
- P1 has root request cap 12, root in-flight model cap 2, child concurrency 2, task deadline 5 minutes. Token usage is observation, not actual-token stop admission.
- The example is read-only for business resources; host may persist immutable artifacts, usage and session logs.
- Default tests/CLI use scripted transport around actual Agent loops. Live model access is opt-in; current user has authorized real validation and environment is sourced from ~/.zshrc without printing secrets.
- No Fork/shared-file executable implementation in P1. Keep those explicit as separately gated phases.
- No push/merge without explicit instruction. Preserve core's unrelated untracked Sandbox files.

## Review Focus

- Old invocation, candidate and dependency results must not be accepted after replacement.
- Correct hashes/citations can still support wrong people/amount/legs; reject the semantic mismatch.
- Parent ctx SessionID must not contaminate child SessionLog/tool identity.
- Root concurrency covers all parent/child clients and releases slots when streams actually finish, including cancellation/error.
- Failed/rejected work remains in request/usage statistics; do not double-count snapshots.

## Task 1: Close review contracts (root owns)

Files: design and review documents above.
- [x] Specify one common authority/qualification protection and consistent lock protocol for normal write, recovery and version updates.
- [x] Resolve I-01 with existing Builder reservation followed by transport limiter; resolve I-02 by request-only hard admission/token observation for P1.
- [x] Clarify ChangeID atomic registration and Fork ProviderState fallback.
- [x] Record which findings are resolved by text vs still awaiting future dynamic W/F verification; render Mermaid and check links/whitespace.

## Task 2: Implement complete isolated read-only example (fresh implementer)

Files owned exclusively by implementer: orchestration/agent-context/* and its new README/fixtures/tests. Do not modify root README or design/review/plan docs.
Interface: `Run(context.Context, Config, Task) (Result, error)`; Config includes Mode (delegation/pipeline/parallel), Dir, optional livemodel client. CLI supports -mode, -dir, -live. Tests include `TestLiveAgentContext` with mode subtests, gated by GOAGENT_LIVE.
- [x] Read full design, apply the explicit I-01/I-02 rulings above; write failing tests before production code.
- [x] Define role briefs retaining why, hard/negative constraints, confirmed decisions, exclusions, evidence refs, output and completion criteria. Separate trusted host envelope from model data.
- [x] Implement immutable local evidence/artifacts and typed reports; validate known synthetic travelers, budget/currency and itinerary fields against real stored evidence rather than model assertions.
- [x] Implement invocation-slot replacement, candidate/upstream/report binding and lock-atomic acceptance; reject stale/canceled/duplicate-id-different-payload results and preserve historical receipts.
- [x] Implement real Builder and native read-only tools in all three modes, independent child SessionID/Dispatcher/lifecycle, role projection, explicit ctx session identity and shared budget/usage hook.
- [x] Implement shared transport limiter (after Builder Reserve; no second reservation), release permits across streaming completion/error/cancel. Test peak and observed root request counts.
- [x] Test privacy and sufficient briefs, decision inheritance, A-vs-B binding, evidence meaning mismatch, unreadable/wrong-version artifact, stale/duplicate adoption, parallel isolation/cancel/error/budget exhaustion, session identity composition and cleanup.
- [x] Offline race and build of new package pass; document exact commands, Mermaid architecture/flow, limits and separate F/W/P2 boundaries. Report TDD RED/GREEN evidence, results and any scope gaps. Do not commit until root has reviewed the artifact, and do not call paid models.

## Task 3: Independent review, integration and verification (root)

- [x] Fresh reviewer checks contract compliance and code quality; fix important findings with regressions.
- [x] Root README links new example. Verify all modules' actual core dependency selection.
- [x] Run examples root race/build, HTTP nested race/build and node protocol tests, core root test and nested frameworkbench test/build; preserve original tests and flags.
- [x] Run new CLI all three modes and flight CLI20 regressions. Run TestLiveAgentContext on Gemini/DeepSeek/MiniMax CN with bounded requests; log only safe evidence and usage.
- [x] If any actual core change becomes necessary, first demonstrate failed contract test, implement a minimal optional extension and validate temporary workspace against all modules plus fixed-version track. Otherwise explicitly report core unchanged.
- [x] Record outcomes and limitations, update design/review implementation status, commit reviewable changes locally, retain feature branch without pushing.
