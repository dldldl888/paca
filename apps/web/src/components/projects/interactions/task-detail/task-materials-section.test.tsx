import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import i18next from "i18next";
import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { loadLanguage } from "@/i18n/config";
import type { Document } from "@/lib/doc-api";

const { mockListDocuments } = vi.hoisted(() => ({
	mockListDocuments: vi.fn(),
}));

vi.mock("@/lib/doc-api", async () => ({
	...(await vi.importActual<typeof import("@/lib/doc-api")>("@/lib/doc-api")),
	listDocuments: mockListDocuments,
}));
vi.mock("@tanstack/react-router", () => ({
	Link: ({
		children,
		params,
	}: {
		children: ReactNode;
		params: { projectId: string; docId: string };
	}) => (
		<a href={`/projects/${params.projectId}/docs/${params.docId}`}>
			{children}
		</a>
	),
}));

import { TaskMaterialsSection } from "./task-materials-section";

const baseDocument: Document = {
	id: "doc-project",
	project_id: "project-1",
	title: "项目手册",
	content: null,
	position: 0,
	created_at: "2026-09-27T00:00:00Z",
	updated_at: "2026-09-27T00:00:00Z",
	visibility: "project",
	access_version: 1,
	task_ids: [],
};

function wrapper({ children }: { children: ReactNode }) {
	return (
		<QueryClientProvider
			client={
				new QueryClient({
					defaultOptions: { queries: { retry: false } },
				})
			}
		>
			{children}
		</QueryClientProvider>
	);
}

describe("TaskMaterialsSection", () => {
	beforeEach(async () => {
		await loadLanguage("zh-CN");
		await i18next.changeLanguage("zh-CN");
		mockListDocuments.mockReset();
	});

	it("shows only readable project materials and materials linked to the current task", async () => {
		mockListDocuments.mockResolvedValue([
			baseDocument,
			{
				...baseDocument,
				id: "doc-task",
				title: "本任务资料",
				visibility: "task",
				task_ids: ["task-1"],
			},
			{
				...baseDocument,
				id: "doc-other-task",
				title: "其他任务资料",
				visibility: "task",
				task_ids: ["task-2"],
			},
			{
				...baseDocument,
				id: "doc-private",
				title: "私人资料",
				visibility: "private",
			},
		]);

		render(<TaskMaterialsSection projectId="project-1" taskId="task-1" />, {
			wrapper,
		});

		expect(
			await screen.findByRole("link", { name: "项目手册" }),
		).toHaveAttribute("href", "/projects/project-1/docs/doc-project");
		expect(screen.getByRole("link", { name: "本任务资料" })).toBeVisible();
		expect(screen.queryByText("其他任务资料")).not.toBeInTheDocument();
		expect(screen.queryByText("私人资料")).not.toBeInTheDocument();
	});
});
