# Public Web Search Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add a private project-draft endpoint that queries a configured public SearXNG-compatible service, returns real URL metadata, and reports provider absence/failure explicitly.

**Architecture:** Keep web results transient until the later preview/material-association task. The draft service owns authorization and calls a small SSRF-safe HTTP client; the handler exposes a bounded query endpoint. Empty provider configuration and non-2xx/malformed upstream responses become a stable 503 error rather than an empty successful search.

**Tech Stack:** Go, net/http, existing `netguard`, chi, current draft service and E2E harness.

**Spec:** `tasks/plan-conversational-project-workbench.md` Task 9 and `tasks/prd-conversational-project-workbench.md` US-018/SC-018.

## Global Constraints

- Public search must not receive private draft text or internal document content.
- Results must expose the source URL, access time, and an unverified/read status.
- Internal/private destinations must be rejected by the existing SSRF-safe transport.
- Provider absence or failure must be visible to the caller; zero results are not provider success when the provider is unavailable.

## Review Focus

- Blank provider configuration returns a service-unavailable error.
- Upstream non-2xx and malformed JSON return service-unavailable without leaking private query data.
- A valid result preserves its public URL and records access time/status.
- Query length and result count are bounded.
- Private loopback provider URLs cannot bypass `netguard`.

### Task 1: Provider contract and service

**Files:**
- Create: `services/api/internal/service/draft/web_search.go`
- Modify: `services/api/internal/service/draft/service.go`
- Test: `services/api/internal/service/draft/web_search_test.go`

- [ ] Write tests for unavailable provider, valid SearXNG JSON, malformed upstream, and bounded query/result parsing.
- [ ] Run the focused test and observe the missing provider behavior fail.
- [ ] Implement the smallest parser/client using `netguard.NewSafeHTTPClient`, returning real URL/title/snippet/access-time/status metadata.
- [ ] Run focused service tests and the draft package suite.

### Task 2: HTTP route and real-stack failure evidence

**Files:**
- Modify: `services/api/internal/config/config.go`
- Modify: `services/api/internal/config/load.go`
- Modify: `services/api/internal/bootstrap/app.go`
- Modify: `services/api/internal/transport/http/handler/draft_handler.go`
- Modify: `services/api/internal/transport/http/router/router.go`
- Modify: `services/api/test/e2e/project_draft_contract_test.go`

- [ ] Add optional `PACA_PUBLIC_SEARCH_URL` wiring and a GET `/project-drafts/web-search` route.
- [ ] Add an E2E assertion that the real stack reports provider unavailability instead of a false empty success.
- [ ] Run targeted Go tests and the existing draft E2E slice.

### Task 3: Evidence and commit

**Files:**
- Create: `/Users/xxp/Desktop/新AI本位/outputs/conversational-project-workbench/run-20260923-task9/task9-public-web-search-report.md`
- Modify: `/Users/xxp/Desktop/新AI本位/docs/migration/acceptance/continuation-progress.md`
- Modify: `/Users/xxp/Desktop/工作日志.md`

- [ ] Record exact passing commands and the current provider limitation.
- [ ] Run `git diff --check`, revert generated route metadata if needed, and commit the bounded slice.
