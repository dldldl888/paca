# Task 1 report: 芯鲜派 brand shell

## Implemented

- Set the default document title and no-logo brand fallback to `芯鲜派`.
- Preserved dynamic workspace `brand_name` and supplied workspace logo URLs.
- Removed the Paca SVG fallback from the employee-facing sidebar, login, set-password, and change-password brand shells. Missing logos render readable text only.
- Updated accessible logo alt text and employee-facing auth, app shell, home, and company assistant labels in the locale files touched by this task. Company assistant labels use `小派`.
- Added focused default-brand and collapsed-sidebar contract tests.

The supplied original Logo was unavailable. The implementation therefore does not present a temporary graphic as the formal Logo; formal Logo acceptance remains unverified.

## Verification

- `pnpm exec vitest run src/components/app-shell/branding-effects.test.ts src/components/app-shell/app-sidebar.test.tsx`: **16 passed**.
- `pnpm exec biome check --write ...`: formatting/import fixes applied; the changed source files then pass the formatter check.
- `pnpm run build`: blocked by three pre-existing type errors outside Task 1 (`unknown` passed where `Error` is required in `src/routes/__root.tsx`, `src/routes/_authenticated/conversations/$conversationId.tsx`, and `src/routes/_authenticated/projects/$projectId/conversations/$conversationId.tsx`).
- `src/routeTree.gen.ts` was restored after test/build generation and is intentionally excluded from the change.

## Concerns

Some technical Paca references remain in project locale hints, paths, shell titles, and browser-extension copy because they are upstream/internal identifiers rather than the brand shell. GitHub URLs, API-key prefixes, and internal Hermes handles were preserved as required.

## Review fix round 1

- Added and used `resolveBrandName()` so the no-logo mark and adjacent wordmark share the same dynamic workspace brand fallback.
- Expanded the focused test to exercise empty, whitespace, default, and custom brand names.
- Removed employee-visible Paca product prose from the English and Simplified Chinese project locale copy. Internal workspace paths and shell titles remain unchanged.

Round 1 verification: focused Vitest remains **16 passed**, Biome passes on changed source files, and `git diff --check` passes.

## Review fix round 2

- Updated `LoginFormPanel` to use `resolveBrandName()` for both the mobile no-logo mark and adjacent wordmark, matching the sidebar and desktop login panel.
- The shared focused test covers custom names plus empty and whitespace-only values, proving the fallback behavior used by the component.

Round 2 verification: focused Vitest **16 passed**, Biome passed, and `git diff --check` passed.
