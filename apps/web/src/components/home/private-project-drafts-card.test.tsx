import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
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
	Link: ({
		children,
		to,
		params,
	}: {
		children: ReactNode;
		to: string;
		params?: { projectId?: string };
	}) => (
		<a href={to.replace("$projectId", params?.projectId ?? "")}>{children}</a>
	),
}));
vi.mock("@/lib/project-draft-api", async () => {
	const actual = await vi.importActual<
		typeof import("@/lib/project-draft-api")
	>("@/lib/project-draft-api");
	return {
		...actual,
		publishProjectDraft: vi.fn(),
	};
});
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
				"home.privateDrafts.preview": "预览",
				"home.privateDrafts.previewReady": "可以确认",
				"home.privateDrafts.previewIssues": "需要补充信息",
				"home.privateDrafts.previewItems": "任务",
				"home.privateDrafts.previewParticipants": "参与人",
				"home.privateDrafts.previewMaterials": "资料",
				"home.privateDrafts.previewManager": "负责人",
				"home.privateDrafts.publish": "确认并分派",
				"home.privateDrafts.publishing": "发布中",
				"home.privateDrafts.published": "已发布",
				"home.privateDrafts.viewProject": "打开项目",
				"home.privateDrafts.publishError": "发布失败，可重试",
				"home.privateDrafts.publishConflict":
					"草稿已发生变化，请刷新后重新预览。",
				"home.privateDrafts.publicationPending": "结果待核实，刷新后可恢复",
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

	it("opens a version-bound preview without publishing", async () => {
		const queryClient = new QueryClient({
			defaultOptions: { queries: { retry: false, staleTime: Infinity } },
		});
		queryClient.setQueryData(
			["private-project-drafts"],
			[
				{
					id: "draft-preview",
					name: "预览项目",
					version: 2,
					state: "editable",
				},
			],
		);
		queryClient.setQueryData(
			["private-project-draft-preview", "draft-preview", 2],
			{
				id: "draft-preview",
				version: 2,
				can_publish: false,
				issues: [{ field: "items", message: "至少添加一项任务" }],
			},
		);
		render(
			<QueryClientProvider client={queryClient}>
				<PrivateProjectDraftsCard />
			</QueryClientProvider>,
		);
		fireEvent.click(screen.getByRole("button", { name: "预览" }));
		expect(await screen.findByText("至少添加一项任务")).toBeInTheDocument();
		expect(screen.getByText("需要补充信息")).toBeInTheDocument();
		expect(screen.queryByText("确认并分派")).not.toBeInTheDocument();
	});

	it("confirms a ready preview with a stable publication id", async () => {
		const { publishProjectDraft } = await import("@/lib/project-draft-api");
		vi.mocked(publishProjectDraft).mockResolvedValue({
			publication_id: "pub-1",
			project_id: "project-1",
			task_ids: { "item-1": "task-1" },
		});
		const queryClient = new QueryClient({
			defaultOptions: { queries: { retry: false, staleTime: Infinity } },
		});
		queryClient.setQueryData(
			["private-project-drafts"],
			[
				{
					id: "draft-ready",
					name: "可发布项目",
					version: 2,
					state: "editable",
					items: [{ id: "item-1" }],
				},
			],
		);
		queryClient.setQueryData(
			["private-project-draft-preview", "draft-ready", 2],
			{
				id: "draft-ready",
				version: 2,
				name: "可发布项目",
				goal: "交付目标",
				items: [{ id: "item-1", title: "可执行任务" }],
				participants: [{ user_id: "user-1" }],
				materials: [],
				can_publish: true,
				issues: [],
			},
		);
		render(
			<QueryClientProvider client={queryClient}>
				<PrivateProjectDraftsCard />
			</QueryClientProvider>,
		);
		fireEvent.click(screen.getByRole("button", { name: "预览" }));
		const publish = await screen.findByRole("button", { name: "确认并分派" });
		fireEvent.click(publish);
		await waitFor(() =>
			expect(publishProjectDraft).toHaveBeenCalledWith(
				"draft-ready",
				2,
				expect.any(String),
			),
		);
		expect(
			(await screen.findAllByText("已发布")).length,
		).toBeGreaterThanOrEqual(1);
		expect(screen.getByRole("link", { name: "打开项目" })).toHaveAttribute(
			"href",
			"/projects/project-1",
		);
	});

	it("shows a deterministic conflict when the draft changed before publish", async () => {
		const { publishProjectDraft } = await import("@/lib/project-draft-api");
		vi.mocked(publishProjectDraft).mockRejectedValueOnce({
			response: { status: 409 },
		});
		const queryClient = new QueryClient({
			defaultOptions: { queries: { retry: false, staleTime: Infinity } },
		});
		queryClient.setQueryData(
			["private-project-drafts"],
			[
				{
					id: "draft-conflict",
					name: "冲突项目",
					version: 2,
					state: "editable",
				},
			],
		);
		queryClient.setQueryData(
			["private-project-draft-preview", "draft-conflict", 2],
			{
				id: "draft-conflict",
				version: 2,
				name: "冲突项目",
				goal: "目标",
				items: [],
				participants: [],
				materials: [],
				can_publish: true,
				issues: [],
			},
		);
		render(
			<QueryClientProvider client={queryClient}>
				<PrivateProjectDraftsCard />
			</QueryClientProvider>,
		);
		fireEvent.click(screen.getByRole("button", { name: "预览" }));
		fireEvent.click(await screen.findByRole("button", { name: "确认并分派" }));
		expect(
			await screen.findByText("草稿已发生变化，请刷新后重新预览。"),
		).toBeInTheDocument();
		expect(
			screen.queryByText("结果待核实，刷新后可恢复"),
		).not.toBeInTheDocument();
	});
});
