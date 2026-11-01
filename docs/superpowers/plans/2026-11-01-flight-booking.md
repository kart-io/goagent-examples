# Flight Booking Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [x]`) syntax for tracking.

**Goal:** Implement a runnable simulated booking example with a real GoAgent Builder loop, a durable workflow and two confirmations.

**Architecture:** A host-owned persistent business record serializes commands per request. Builder extracts intent and fields; a DurableGraph handles search, selection, final authorization, payment, issuance and compensation. Independent simulated supplier receipts survive graph checkpoint loss.

**Tech Stack:** Go 1.25, existing GoAgent pin, core/durable FileStore, workflow.Graph, builder, existing livemodel adapter.

**Spec:** docs/superpowers/specs/2026-11-01-flight-booking-design.md (approved by user 2026-11-01).

## Global Constraints

- No core changes or additional dependencies; all supplier effects are simulated.
- First confirmation selects itinerary; second authorizes exact final snapshot.
- Any changed itinerary, passenger, price, currency or rule invalidates old authorization.
- Unknown payment/ticket/refund results require reconciliation with stable business keys.
- Authenticated identity is a host concern; demo actor strings are not authentication.
- No automatic pushes or releases.

## Review Focus

- Replaying a previous revision must not pay the new revision.
- Completed effects remain discoverable after approval expiry.
- Cancellation racing confirmation must not conceal a committed payment.
- Reusing a request ID with another owner or payload must fail.
- Malformed model output must not authorize business effects.

## Task 1: Business contract and scenario tests

Files: orchestration/flight-booking/types.go, booking_test.go, supplier.go.
Interfaces: Request, Command, Config; Run(context.Context, Config, Command) (Result, error).
- [x] Write integration tests for two-stage confirmations, invalid identity/token, stale quotes, cancellation, revisions, concurrency and unknown effects. Assert stored business receipts rather than mock call counts.
- [x] Run GOWORK=off go test ./orchestration/flight-booking; expect missing implementation failure.
- [x] Implement typed request validation, synthetic offers, persistent supplier operations and confirmation snapshots.
- [x] Run the scenario tests; expect PASS.

## Task 2: Durable workflow and host commands

Files: booking.go, graph.go, recovery_test.go.
Consumes: Task 1 typed records. Produces Run and revision-bound graph.
- [x] Add crash subprocess tests for payment, ticket and refund commit-before-checkpoint; first run must expose missing recovery.
- [x] Implement search -> choose -> prepare -> pay -> issue -> settle; approval nodes choose/pay; host owns intent and authorization tokens and revision records.
- [x] Test stale revisions, duplicate commands, owner/payload conflicts and cancel/payment races.
- [x] Run GOWORK=off go test -race ./orchestration/flight-booking; expect PASS.
- [x] Commit business workflow with tests.

## Task 3: Agent and CLI

Files: agent.go, agent_test.go, main.go, cli_test.go, fixtures/*.json.
Consumes: Run and validated Request. Produces CLI with start/message/select/confirm/status/revise/cancel and optional live model.
- [x] Add failing Agent tests for clarification, complete extraction, unsupported request, malicious/invalid output and usage recording.
- [x] Use actual Builder.AsRunnable with bounded model calls; scripted transport only supplies fixture responses. Persist conversation and use budget usage hook for invocation evidence and durable root accounting.
- [x] Add CLI invocation test, then implement flags with explicit action/token/actor and input files. Fault flags inject supplier errors/crashes only; no real API effects.
- [x] Run offline Agent/CLI tests and build with GOWORK=off; expect PASS.
- [x] Commit Agent/CLI and fixtures.

## Task 4: Documentation and final verification

Files: README.md, orchestration/flight-booking/README.md, docs/evaluation/2026-11-01-flight-booking/REPORT.md.
- [x] Document model-vs-workflow responsibility, two confirmations, all CLI paths, scenario table, production adapter boundaries and unsupported active changes/refunds.
- [x] Run documented CLI commands including a fresh-process crash/recovery; record real results.
- [x] Run GOWORK=off go test -race ./... and go build ./...; no paid model tests enabled.
- [x] Fresh whole-change review; resolve important findings with regression tests.
- [x] Record coverage/limitations and commit docs and evidence. Leave branch ready for review without push.

## Execution rulings

- Native execution in a feature branch, as authorized by the user's instruction to execute; no additional execution-method approval loop.
- Persist the business record and supplier receipt ledger independently from graph checkpoints. Per-request file locks serialize all host commands and graph transitions.
- Workflow confirmation node approval keys stay internal; public tokens are random and bound to business snapshots. The host persists accepted authorization before advancing the graph.

## Completion evidence

- Root race: 35 packages passed, 7 without tests; /tmp/flight-booking-full-race.log.
- New-package offline race: PASS, including independent-process crash tests.
- Root go build ./...: exit 0.
- Compiled CLI smoke: 20 cases passed; no live/paid model calls.
- Independent whole-change review found 3 correctness issues, all fixed with RED/GREEN regressions; review wording correction also applied.
- Task 1–3 code shares one coherent integration commit, as recorded in execution rulings.
