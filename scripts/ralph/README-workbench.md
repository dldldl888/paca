# Employee workbench acceptance

Use the committed Bun lockfiles. Installing a fresh pnpm dependency tree changes
assistant-ui internals and does not reproduce the deployed runtime.

With the dedicated synthetic stack running on `http://127.0.0.1:3400`:

```sh
export E2E_BASE_URL=http://127.0.0.1:3400
export E2E_WORKBENCH_FIXTURE=/private/path/workbench-fixture.json
export E2E_WORKBENCH_OUTPUT=/path/to/evidence/browser
export WORKBENCH_REPORT_DIR=/path/to/evidence
# Set BUN=/path/to/bun when bun is not on PATH.
scripts/ralph/verify_workbench.sh /path/to/prd-xianxianpai-workbench.json
```

The command installs frozen dependencies, runs all web tests and the complete
TypeScript/Vite build, the affected API/bridge packages in the existing dedicated
Docker environment, tests the specification validator, and executes every
active scenario and its `additionalTests`. It exits nonzero for any failure.
The planning project's PRD remains the single source of truth; pass its path.

Browser tests use the existing Playwright package with the separate
`apps/e2e/workbench.config.ts`. They require a fixture that explicitly declares
`synthetic: true`, employee/manager/admin test account credentials, and the
task/conversation identifiers listed in that config. Each browser context logs
in through the real API; the main workflow also tests the visible login form.
Keep this fixture private and outside Git. Real model access and an online
authenticated bridge are required.
Missing prerequisites fail the tests instead of skipping them.

The suite preserves old feedback, appends labeled synthetic acceptance comments,
and restores the task status. It uses actual HTTP and browser flows; it does not
mock permission rejection, persistence, model replies, or cancellation.

Scenario test identifiers are `repository/path::full describe and test name`.
The validator verifies the imported SUT reference and an assertion matcher in
the exact test body, then checks the native runner's executed result. This is a
structural guard, not proof that arbitrary assertions are semantically adequate;
the contract tests and their boundary fixtures still require code review.
