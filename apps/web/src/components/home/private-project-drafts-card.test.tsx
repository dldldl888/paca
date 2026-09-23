import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import type { ReactNode } from "react";
import { describe, expect, it, vi } from "vitest";

const mocks = vi.hoisted(() => ({ canCreate: true }));

vi.mock("@/hooks/use-permissions", () => ({
	usePermissions: () => ({
		hasPermission: (permission: string) =>
			permission === "projects.create" && mocks.canCreate,
	}),
}));
vi.mock("@tanstack/react-router", () => ({
	Link: ({ children, to }: { children: ReactNode; to: string }) => (
		<a href={to}>{children}</a>
	),
}));
vi.mock("react-i18next", () => ({
	useTranslation: () => ({
		t: (key: string, values?: { count?: number; version?: number }) =>
			({
				"home.privateDrafts.title": "私人项目草稿",
				"home.privateDrafts.openChat": "继续在小派中讨论",
				"home.privateDrafts.empty": "还没有私人项目草稿",
				"home.privateDrafts.loading": "草稿加载中",
				"home.privateDrafts.error": "草稿暂时无法加载",
				"home.privateDrafts.version": `第 ${values?.version} 版`,
				"home.privateDrafts.status.editable": "可编辑",
				"home.privateDrafts.status.generating": "生成中",
				"home.privateDrafts.status.failed": "需要重试",
				"home.privateDrafts.status.imported": "已更新",
			})[key] ?? key,
	}),
}));

import { PrivateProjectDraftsCard } from "./private-project-drafts-card";

function renderCard(items: unknown[]) {
	const queryClient = new QueryClient({
		defaultOptions: { queries: { retry: false, staleTime: Infinity } },
	});
	queryClient.setQueryData(["private-project-drafts"], items);
	return render(
		<QueryClientProvider client={queryClient}>
			<PrivateProjectDraftsCard />
		</QueryClientProvider>,
	);
}

describe("PrivateProjectDraftsCard", () => {
	it("shows draft versions and generation status with a chat continuation link", async () => {
		renderCard([
			{
				id: "draft-1",
				name: "新品发布",
				version: 3,
				state: "editable",
				generation: { status: "running" },
			},
		]);
		expect(
			await screen.findByRole("region", { name: "私人项目草稿" }),
		).toBeInTheDocument();
		expect(screen.getByText("新品发布")).toBeInTheDocument();
		expect(screen.getByText("第 3 版")).toBeInTheDocument();
		expect(screen.getByText("生成中")).toBeInTheDocument();
		expect(
			screen.getByRole("link", { name: "继续在小派中讨论" }),
		).toHaveAttribute("href", "/conversations");
	});

	it("does not expose private drafts without the create-project capability", () => {
		mocks.canCreate = false;
		renderCard([
			{ id: "draft-1", name: "不可见", version: 1, state: "editable" },
		]);
		expect(screen.queryByText("不可见")).not.toBeInTheDocument();
		mocks.canCreate = true;
	});
});
