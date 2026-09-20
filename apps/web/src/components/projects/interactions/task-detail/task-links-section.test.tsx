import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import i18next from "i18next";
import type { ReactNode } from "react";
import { expect, it, vi } from "vitest";
import { loadLanguage } from "@/i18n/config";

const mocks = vi.hoisted(() => ({ listTaskLinks: vi.fn() }));

vi.mock("@/lib/interaction-api", async () => {
	const actual = await vi.importActual<typeof import("@/lib/interaction-api")>(
		"@/lib/interaction-api",
	);
	return {
		...actual,
		taskLinksQueryOptions: (projectId: string, taskId: string) => ({
			queryKey: ["projects", projectId, "tasks", taskId, "links"],
			queryFn: mocks.listTaskLinks,
		}),
	};
});

vi.mock("./add-task-link-modal", () => ({
	AddTaskLinkModal: () => null,
}));

import { TaskLinksSection } from "./task-links-section";

function wrapper({ children }: { children: ReactNode }) {
	return (
		<QueryClientProvider
			client={
				new QueryClient({ defaultOptions: { queries: { retry: false } } })
			}
		>
			{children}
		</QueryClientProvider>
	);
}

it.each([
	["zh-CN", "等待前置工作完成"],
	["en", "Waiting for prerequisite work to finish"],
] as const)("shows the anonymous hidden blocker state in %s", async (language, text) => {
	await loadLanguage(language);
	await i18next.changeLanguage(language);
	mocks.listTaskLinks.mockResolvedValue({
		items: [],
		waiting_on_hidden_blocker: true,
	});

	render(<TaskLinksSection projectId="project-a" taskId="task-a" />, {
		wrapper,
	});

	expect(await screen.findByText(text)).toBeInTheDocument();
	expect(
		screen.queryByText(/No linked tasks|暂无关联任务/),
	).not.toBeInTheDocument();
	expect(
		screen.queryByRole("button", { name: /Open task|打开任务/ }),
	).not.toBeInTheDocument();
});
