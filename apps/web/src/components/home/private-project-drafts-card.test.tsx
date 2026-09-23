import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen } from "@testing-library/react";
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
				"home.privateDrafts.showMaterials": "查找内部资料",
				"home.privateDrafts.hideMaterials": "收起内部资料",
				"home.privateDrafts.materialSearch": "搜索内部资料",
				"home.privateDrafts.materialSearchPlaceholder": "按标题搜索…",
				"home.privateDrafts.search": "搜索",
				"home.privateDrafts.materialLoading": "资料加载中",
				"home.privateDrafts.materialError": "资料暂时无法加载",
				"home.privateDrafts.materialEmpty": "没有找到当前可读取的内部资料",
				"home.privateDrafts.showWebSearch": "搜索公开网页",
				"home.privateDrafts.hideWebSearch": "收起公开网页",
				"home.privateDrafts.webSearch": "搜索公开网页",
				"home.privateDrafts.webSearchPlaceholder": "输入公开查询…",
				"home.privateDrafts.webSearchLoading": "公开检索中",
				"home.privateDrafts.webSearchError": "公开检索暂时不可用",
				"home.privateDrafts.webSearchEmpty": "没有找到公开结果",
			})[key] ?? key,
	}),
}));

import { PrivateProjectDraftsCard } from "./private-project-drafts-card";

function renderCard(
	items: unknown[],
	chatSessionId?: string,
	materials: unknown[] = [],
) {
	const queryClient = new QueryClient({
		defaultOptions: { queries: { retry: false, staleTime: Infinity } },
	});
	queryClient.setQueryData(["private-project-drafts"], items);
	queryClient.setQueryData(["private-project-draft-materials", ""], materials);
	return render(
		<QueryClientProvider client={queryClient}>
			<PrivateProjectDraftsCard chatSessionId={chatSessionId} />
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

	it("filters the card to the active private chat session", async () => {
		renderCard(
			[
				{
					id: "draft-1",
					name: "当前会话草稿",
					chat_session_id: "session-current",
					version: 1,
					state: "editable",
				},
				{
					id: "draft-2",
					name: "别的会话",
					chat_session_id: "session-other",
					version: 1,
					state: "editable",
				},
			],
			"session-current",
		);
		expect(screen.getByText("当前会话草稿")).toBeInTheDocument();
		expect(screen.queryByText("别的会话")).not.toBeInTheDocument();
	});

	it("reveals readable material search with source status", async () => {
		const queryClient = new QueryClient({
			defaultOptions: { queries: { retry: false, staleTime: Infinity } },
		});
		queryClient.setQueryData(["private-project-drafts"], []);
		queryClient.setQueryData(
			["private-project-draft-materials", "路线"],
			[
				{
					id: "doc-1",
					title: "可读路线图",
					processed_status: "ready",
				},
			],
		);
		const rerender = render(
			<QueryClientProvider client={queryClient}>
				<PrivateProjectDraftsCard />
			</QueryClientProvider>,
		);
		fireEvent.click(screen.getByRole("button", { name: "查找内部资料" }));
		fireEvent.change(screen.getByRole("textbox", { name: "搜索内部资料" }), {
			target: { value: "路线" },
		});
		fireEvent.click(screen.getByRole("button", { name: "搜索" }));
		expect(await screen.findByText("可读路线图")).toBeInTheDocument();
		expect(screen.getByText("ready")).toBeInTheDocument();
		rerender.unmount();
	});

	it("shows public web candidates with unverified status", async () => {
		const queryClient = new QueryClient({
			defaultOptions: { queries: { retry: false, staleTime: Infinity } },
		});
		queryClient.setQueryData(["private-project-drafts"], []);
		queryClient.setQueryData(
			["private-project-draft-web-search", "公开"],
			[
				{
					title: "公开页面",
					url: "https://example.com/page",
					snippet: "公开摘要",
					read_status: "unverified",
					processed_status: "unverified",
					accessed_at: "2026-09-23T00:00:00Z",
				},
			],
		);
		render(
			<QueryClientProvider client={queryClient}>
				<PrivateProjectDraftsCard />
			</QueryClientProvider>,
		);
		fireEvent.click(screen.getByRole("button", { name: "搜索公开网页" }));
		fireEvent.change(screen.getByRole("textbox", { name: "搜索公开网页" }), {
			target: { value: "公开" },
		});
		fireEvent.click(screen.getByRole("button", { name: "搜索公开网页" }));
		expect(await screen.findByText("公开页面")).toBeInTheDocument();
		expect(screen.getByText(/unverified/)).toBeInTheDocument();
		expect(screen.getByRole("link", { name: "公开页面" })).toHaveAttribute(
			"href",
			"https://example.com/page",
		);
	});
});
