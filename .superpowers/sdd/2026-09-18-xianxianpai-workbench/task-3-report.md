# Task 3 report: employee home and manager workspace entry

Status: implemented.

The authenticated home now mounts `XianxianpaiHome` once before the existing project dashboard. It orders welcome, assigned tasks, notifications, Xiaopai, company materials, and organization navigation, with distinct Chinese loading, empty, and error guidance. Company materials resolve only to the existing `公司资料` or `总经办` project, then use the existing permitted document list and link to a real document detail; no guessed project or redirecting docs root is used. Xiaopai availability comes from the existing chattable-agent query. `ManagerWorkspaceEntry` uses the existing global `tasks.manage` and project `tasks.manage` permission sources; leader names do not grant access. Authorized projects expose existing drafts, backlog, and team routes with project-scoped region labels.

Evidence:

- `pnpm exec vitest run src/components/home/xianxianpai-home.test.tsx src/components/home/manager-workspace-entry.test.tsx src/components/home/assigned-tasks-list.test.tsx src/components/company/organization-navigation.test.tsx`: 4 files, 12 tests passed.
- `./node_modules/.bin/tsc --noEmit`: passed.
- Biome check on all changed source, tests, route, and locale files: passed.
- `git diff --check`: passed.
- `apps/web/src/routeTree.gen.ts` was restored before commit; no generated route-tree change is included.
- Focused tests cover the real `/projects/company/docs/doc-1` target and explicit project, notification, and Xiaopai loading/unavailable states.

The full `pnpm run build` still stops at the three known baseline type errors, all outside this task: `src/routes/__root.tsx:19`, `src/routes/_authenticated/conversations/$conversationId.tsx:19`, and `src/routes/_authenticated/projects/$projectId/conversations/$conversationId.tsx:22` (`unknown` is not assignable to `Error`).

The supplied formal Logo remains unavailable and is not represented as an accepted asset by this task.
