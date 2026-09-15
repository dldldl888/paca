# Paca Context Recovery and Isolation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Restore native Hermes conversations without replay duplication and enforce current object authorization across Paca and per-conversation OS sandboxes.

**Architecture:** Reuse the API authorizer and task repository predicate; carry authenticated identity through backend dispatch. Give each private conversation its own native process and state, save only a trusted atomic session binding, and use ACP session/load after process loss. Realtime asks the API for current recipients before delivering sensitive events.

**Tech Stack:** Existing Go modules, Bun/TypeScript, PostgreSQL/Valkey, Python standard library, native Hermes ACP and macOS sandbox-exec. No new application dependencies.

**Spec:** tasks/prd-context-recovery-isolation.md and scripts/ralph/prd.json (15 stories, 31 behavior scenarios).

## Global Constraints

- Work only in the existing linked worktree on codex/context-recovery-isolation; baseline 59e871b2909e43e75dc360e9187698c1460ca7fd.
- User has authorized execution, tests, reviews and dedicated local integration; no Ralph loop, repeated execution-choice question, shared push, merge or production deployment.
- Current assignee or authenticated immutable creator may read; tasks.read_all and tasks.manage require the existing corresponding read/write permission and current project qualification. Reporter is never creator. Missing identity fails closed.
- Task management does not grant access to another user's private conversations.
- Filters intersect authorization before pagination and aggregate computation. Reject writes before side effects.
- Private identity binds instance, agent, project, owner and conversation; task/agent scope cannot merge it. Fail recovery instead of silently creating an empty context.
- Preserve native tools inside the conversation; real OS enforcement must block sibling state, personal data, traversal and symlinks. Never pass bridge/admin credentials to Hermes.
- Preserve existing busy, queue, stop, stream, reconnect behavior. No capacity expansion or UI redesign.
- Historical mixed Hermes home stays intact. No copying its database to new users.
- Every acceptance result needs actual test execution; no zero-test or file-string pass. Preserve failures/skips honestly. Credentials remain only in private runtime storage.
- Independent correctness/spec review and separate Ponytail review for each deliverable. Real reversible ablation only when a candidate exists; use identical relevant checks before/after and record the decision.

## Test environment and initial evidence

Use transient containers mounting this worktree at /src, not the running deployment source. Existing images paca-dev-api and paca-dev-agent-runner contain the Go toolchain; oven/bun:1-alpine contains Bun. Go flags: GOFLAGS=-p=2 and CGO_ENABLED=1 for SQLite tests/race builds. Baseline logs: /tmp/paca-recovery-evidence/. E2E uses dedicated ephemeral infrastructure; do not reset paca-dev data. Native Python for Hermes is /Users/xxp/.hermes/hermes-agent/venv/bin/python.

## Task 1: ACP load protocol (US-011)

**Files:** Modify apps/acp-bridge/internal/acpclient/acpclient.go and acpclient_test.go only.
**Interfaces:** Preserve Initialize(ctx) error. Add SupportsLoadSession() bool, LoadSession(ctx context.Context, sessionID, cwd string) (*SessionModeState, error), and Alive() bool for the recovery consumer.

- [ ] Add TestUS011LoadProtocol using the existing pipe fakePeer. Initialize advertises loadSession; call LoadSession with sess-A and a fixed cwd, assert session/load parameters including empty mcpServers, send update before response, assert callback order and absence of session/new.
- [ ] Add TestUS011LoadFailures: unsupported capability, null/scalar/malformed response, RPC error, missing/invalid input and timeout must return error. Test meaningful empty object response (ACP load response fields are optional) against native schema. Initialize must decode capabilities rather than ignore them.
- [ ] Run focused tests and capture expected RED; implement minimal JSON decoding and capability check. Load never calls NewSession. Alive reads the existing closed channel non-blockingly.
```go
select { case <-c.closed: return false; default: return true }
```
- [ ] Run the acpclient package, then full bridge tests; record RED/GREEN commands and evidence. Commit only these files. Review protocol compliance, errors and Ponytail complexity.

## Task 2: Task object boundary and all retained API surfaces (US-001 through US-006)

**Files:** services/api/internal/platform/authz/{permissions,authorizer}.go; domain/task/{entity,repository}.go; repository/postgres/{task_repository,authz_permission_store}.go; service/task/{task_service,activity_service,task_link_service,cached_service}.go; service/attachment/service.go; service/annotation/service.go; transport/http/{middleware,handler,dto} affected task paths; cmd/api/main.go; next numbered migration and focused existing tests.
**Interfaces:** Common task access resolves authenticated actor from request context and live DB membership/permissions. Expose an access check reusable by attachments, related content, chat and internal recipients. Extend TaskFilter with server-only visibility scope, never bind it from JSON. CreatorUserID is server-owned and nullable for old rows.

- [ ] Trace every caller before editing; record exact API decisions and signature deltas in the task report. Follow service/repository patterns, avoiding per-handler role logic.
- [ ] Add actual tests named for US001-006 (adapt prd.json bindings later if integration tests offer stronger evidence). Assert old creator NULL; manager scope vs employee; A/B ID and number reads; creator/reporter/removal; full pagination/search/count/sum; mixed-field update is all-or-nothing; manager-only create/assign/delete; activity/link/annotation and attachment read/write refusal before storage/DB effects.
```go
// Behavior expectations, using existing real service/repository fixtures:
if err == nil { t.Fatal("unauthorized task operation succeeded") }
if count != 0 { t.Fatalf("restricted rows affected: %d", count) }
```
- [ ] Capture RED in focused packages. Add nullable creator migration without reporter backfill, permission registration, live common policy and SQL predicate. Preserve internal callers only with explicit bounded service context. Ensure caches cannot reuse one user's task results for another.
- [ ] Apply policy to task siblings, home/sprint/view aggregates, related endpoints and storage signing/completion. Reuse the same predicate for List/Count/Sum.
- [ ] Run affected Go suites and dedicated Postgres integration behavior; commit reviewed code. Record omitted independent followups explicitly instead of claiming whole story completion.

## Task 3: Chat authorization and backend identity (US-008, US-009 backend, US-014 queue)

**Files:** services/api/internal/service/agent/agent_service.go and related chat functions; repository/postgres conversation lookup; services/agent-runner/internal/{agent,acpbridge,messaging,repository/postgres}; API internal authorization endpoint/router wiring and tests.
**Interfaces:** Trusted dispatch fields audience, owner_user_id, agent_id, instance_id, project_id, conversation_id and has_prior_turns come from the persisted conversation, never the incoming browser/queue actor. Keep message/task context building after current authorization. An internal authenticated API check supplies private run authorization to agent-runner and realtime; no duplicate role engine.

- [ ] Write RED tests for inaccessible task references, manager unable to read/control/search another private chat, trigger actor spoofing, queued run after membership/assignment revocation, and missing private ownership.
- [ ] Implement common task check at chat start/continue, protect history by existing owner semantics, derive dispatch identity from DB, and recheck queued runs before building context or starting the bridge. Unsupported autonomous tasks in the dedicated enterprise private mode fail explicitly.
- [ ] Assert rejected run has failed status, no prompt, no task material and no automatic broad-credential fallback. Run related API/agent-runner tests and commit/review.

## Task 4: Per-conversation Hermes runtime and atomic recovery (US-009 bridge, US-010, US-012, US-013)

**Files:** apps/acp-bridge/internal/runner/{runner.go,session_scope_test.go,runner_test.go,session_store.go,session_store_test.go}; internal/cli/cli.go; internal/acpclient spawn configuration if needed; scripts/{launch_hermes.py,test_hermes_isolation.py}; README.md.
**Interfaces:** Private key is a stable digest of validated instance/agent/project/owner/conversation. Config supplies trusted state root outside cwd. Store versioned identity/sessionID/cwd metadata atomically outside model-readable directories. Trusted launcher receives only validated runtime paths plus allowlisted runtime/config and fresh environment.

- [ ] Test private keys differ for same task across owners/conversations even under task/agent scope. Reject missing/mismatched identity, symlink/traversal roots. Preserve separate legacy shared namespace outside enterprise mode.
- [ ] Test new session commits mapping before first prompt; write failure prevents prompt and preserves prior file. Load rejects corrupted metadata, owner/project/cwd/version mismatch and symlinks. Use os.OpenRoot or similarly anchored operations, JSON, temp file, fsync, rename and restrictive modes; no new store service.
- [ ] Real Python tests construct separate homes and run native session_search/history/memory and direct native session-load under sandbox-exec, plus file/process/symlink attempts. A/B tools work locally and cannot read each other's markers or personal files. Do not whitelist the whole runtime root or entire personal Hermes home.
- [ ] Test rebuilt Runner, dead subprocess and idle eviction load original ID; load replay updates are suppressed until load completes and future updates must match active session. Missing old mapping/native session/capability/invalid result/timeout fails with no empty prompt.
```go
if state.loading || update.SessionID != state.sessionID { return }
```
- [ ] Keep each turn serial and bound initialization/load deadlines. Preserve mapping/state during eviction. Run full bridge tests (including race where supported) and real sandbox checks, then commit/review.

## Task 5: Current realtime recipients and running revocation (US-007, US-014 live)

**Files:** services/realtime/src/{subscriber,server,auth,permissions,config}.ts and subscriber.test.ts; API authenticated internal current-recipient handler and task/common auth integration; agent-runner bridge event ingestion/dispatch control; API notification consumer.
**Interfaces:** routeEvent queries common API authorization at delivery time. Sensitive task/related/shared-agent events deliver only to authorized user rooms. Private task chat output is still owner-only and additionally checks current task/project run scope. Missing recipient data/lookup errors never cause project broadcast.

- [ ] Write Bun routing RED tests for recipient A once/B zero, missing scopes and failed lookups, and old socket/queued event after revoke; assert emitted payloads, not string presence.
- [ ] Wire current-recipient lookup using existing service authentication. Ensure user room event channel still matches browser subscriptions. Avoid stale ACL/role caching and late async events overtaking revocation.
- [ ] Recheck private run authorization on incoming chunks/control; stop and fail revoked runs. Drop later output and don't persist newly unauthorized task material. Gate new notification content with current common policy.
- [ ] Run Bun check/tests and focused Go tests. Commit/review with independent revocation reasoning.

## Task 6: Auditable binding gate and dedicated full acceptance (US-015 and all prior scenarios)

**Files:** scripts/ralph/{validate_spec.py,test_validate_spec.py,prd.json}; dedicated validation scripts under apps/acp-bridge/scripts or outputs in the document project; docs acceptance/rollback/ablation record.
**Interfaces:** validate_story resolves native Go/Bun/Python test bodies and bound SUT calls, actually runs selected test nodes, rejects zero tests/skip/missing/empty bodies. Scope is selected current story plus completed stories; final covers all 31 scenarios. No file-wide substring acceptance.

- [ ] Add RED tests with missing symbol/test, empty test, body unrelated to SUT, valid invocation and subsequent completed-story drift. Use Python AST for Python, Go parser for Go and TypeScript compiler parser already installed for Bun if needed; never add a new parser framework.
- [ ] Update bindings only to genuine tests; preserve every given/when/then. Native execution output is proof; binding checks alone never prove security.
- [ ] Before switching local API/realtime/agent-runner/bridge, backup dedicated database and private configuration with mode 0700. Store raw backups privately and public manifests without secrets. Preserve old deployment source and base images, use explicit compose override for this worktree only.
- [ ] Reuse old synthetic resources/scripts without overwriting old failures. Run 8 private chat checks, all 13 real model lifecycle checks plus child exit/idle/replay/failure injections, A/B task endpoints/query/aggregation/attachments/related writes/realtime, same-task private native tools, revocation while queued/streaming/reconnect.
- [ ] Browser verify two identities, own tasks/notifications, forbidden other task, private continuation/status. Preserve preexisting provider401 and devtools mouse-overlay limitation if not fixed.
- [ ] Record exact commands, versions, exits, assertions, untested/failed items, review and real ablation comparisons. Only set a story passes=true when all its behavior and required live checks pass.
- [ ] Run final affected Go/Bun/build/binding gates, independent whole-branch review, fix verified findings and write concrete rollback procedure. Keep local branch/worktree for user review; no push/merge.
