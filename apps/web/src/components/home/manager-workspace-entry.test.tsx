import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import type { ReactNode } from "react";
import { describe, expect, it, vi } from "vitest";

const mocks = vi.hoisted(() => ({
	globalManage: false,
	projectManage: new Set<string>(),
}));

vi.mock("@/hooks/use-permissions", () => ({
	usePermissions: () => ({
		hasPermission: (permission: string) =>
			permission === "tasks.manage" && mocks.globalManage,
	}),
}));
vi.mock("@/hooks/use-project-permissions", () => ({
	useProjectPermissions: (projectId: string) => ({
		hasProjectPermission: (permission: string) =>
			permission === "tasks.manage" && mocks.projectManage.has(projectId),
	}),
}));
vi.mock("@/lib/project-api", () => ({
	projectsQueryOptions: () => ({ queryKey: ["manager-projects"] }),
}));
vi.mock("@tanstack/react-router", () => ({
	Link: ({
		children,
		to,
		params,
	}: {
		children: ReactNode;
		to: string;
		params?: Record<string, string>;
	}) => (
		<a href={params ? to.replace("$projectId", params.projectId ?? "") : to}>
			{children}
		</a>
	),
}));
vi.mock("react-i18next", () => ({
	useTranslation: () => ({
		t: (key: string, values?: { project?: string }) =>
			({
				"home.manager.title": "负责人工作台",
				"home.manager.goals": `团队目标与草稿 · ${values?.project}`,
				"home.manager.tasks": `团队任务 · ${values?.project}`,
				"home.manager.feedback": `从任务查看反馈 · ${values?.project}`,
			})[key] ?? key,
	}),
}));

import { ManagerWorkspaceEntry } from "./manager-workspace-entry";

function renderEntry(projects: unknown[]) {
	const queryClient = new QueryClient({
		defaultOptions: { queries: { retry: false } },
	});
	queryClient.setQueryData(["manager-projects"], {
		items: projects,
		total: projects.length,
	});
	return render(
		<QueryClientProvider client={queryClient}>
			<ManagerWorkspaceEntry />
		</QueryClientProvider>,
	);
}

const project = (id: string, name: string) => ({ id, name });

describe("ManagerWorkspaceEntry", () => {
	it("shows role-scoped management links for a project manager", async () => {
		mocks.globalManage = false;
		mocks.projectManage = new Set(["engineering"]);
		renderEntry([
			project("engineering", "工程部"),
			project("finance", "财务部"),
		]);

		expect(
			await screen.findByRole("link", { name: "团队目标与草稿 · 工程部" }),
		).toHaveAttribute("href", "/projects/engineering/drafts");
		expect(
			screen.getByRole("region", { name: "负责人工作台 · 工程部" }),
		).toBeInTheDocument();
		expect(
			screen.getByRole("link", { name: "团队任务 · 工程部" }),
		).toHaveAttribute("href", "/projects/engineering/interactions/backlog");
		expect(
			screen.getByRole("link", { name: "从任务查看反馈 · 工程部" }),
		).toHaveAttribute("href", "/projects/engineering/interactions/backlog");
		expect(screen.queryByText("财务部")).not.toBeInTheDocument();
	});

	it("hides all management links for an employee even when leaders are present", async () => {
		mocks.globalManage = false;
		mocks.projectManage = new Set();
		renderEntry([project("engineering", "工程部")]);

		await new Promise((resolve) => setTimeout(resolve, 0));
		expect(
			screen.queryByRole("region", { name: "负责人工作台" }),
		).not.toBeInTheDocument();
		expect(
			screen.queryByRole("link", { name: /团队目标|团队任务|从任务查看反馈/ }),
		).not.toBeInTheDocument();
	});
});
