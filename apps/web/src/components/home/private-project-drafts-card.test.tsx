import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { ProjectDraft, ProjectDraftFile } from "@/lib/project-draft-api";

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
		updateProjectDraft: vi.fn(),
		uploadProjectDraftFile: vi.fn(),
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
				"home.privateDrafts.materialAttach": "关联到当前草稿",
				"home.privateDrafts.materialTask": "资料任务关联",
				"home.privateDrafts.materialTaskRequired":
					"至少选择一项任务后才能关联资料",
				"home.privateDrafts.materialAttachError":
					"资料关联保存失败，请刷新后重试。",
				"home.privateDrafts.upload": "上传文件",
				"home.privateDrafts.uploading": "上传中…",
				"home.privateDrafts.uploadError": "上传或解析失败，请重试。",
				"home.privateDrafts.retryUpload": "重试上传",
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

const materialDraft: ProjectDraft = {
	id: "draft-material",
	name: "资料关联项目",
	goal: "目标",
	items: [
		{
			id: "item-1",
			title: "准备报告",
			requirements: "整理数据",
			criteria: "交付报告",
			importance: 1,
			dependencies: [],
		},
		{
			id: "item-2",
			title: "审核报告",
			requirements: "审核",
			criteria: "签收",
			importance: 1,
			dependencies: [],
		},
	],
	participants: [],
	materials: [],
	chat_session_id: "session-1",
	version: 1,
	state: "editable",
	created_at: "2026-09-24T00:00:00Z",
	updated_at: "2026-09-24T00:00:00Z",
};
const readyFile: ProjectDraftFile = {
	id: "draft-file-1",
	draft_id: materialDraft.id,
	file_id: "file-1",
	file_name: "brief.pdf",
	content_type: "application/pdf",
	file_size: 12,
	upload_status: "uploaded",
	processing_status: "ready",
	created_at: "2026-09-24T00:00:00Z",
	updated_at: "2026-09-24T00:00:00Z",
};

function renderMaterialDraft(
	draft = materialDraft,
	files: ProjectDraftFile[] = [],
	otherDrafts: ProjectDraft[] = [],
) {
	const queryClient = new QueryClient({
		defaultOptions: { queries: { retry: false, staleTime: Infinity } },
	});
	queryClient.setQueryData(["private-project-drafts"], [draft, ...otherDrafts]);
	for (const entry of [draft, ...otherDrafts]) {
		queryClient.setQueryData(
			["private-project-draft-preview", entry.id, entry.version],
			{ ...entry, can_publish: false, issues: [], employee_views: [] },
		);
		queryClient.setQueryData(["private-project-draft-files", entry.id], files);
	}
	queryClient.setQueryData(
		["private-project-draft-materials", ""],
		[
			{
				id: "doc-1",
				title: "可读路线图",
				source_type: "internal",
				project_id: "source-project",
				source_location: "/projects/source-project/docs/doc-1",
				source_version: "2026-09-24T00:00:00Z",
				processed_status: "ready",
				visibility: "project",
			},
		],
	);
	render(
		<QueryClientProvider client={queryClient}>
			<PrivateProjectDraftsCard />
		</QueryClientProvider>,
	);
	fireEvent.click(screen.getAllByRole("button", { name: "预览" })[0]);
	fireEvent.click(screen.getByRole("button", { name: "查找内部资料" }));
	return queryClient;
}

describe("PrivateProjectDraftsCard material association", () => {
	beforeEach(() => vi.clearAllMocks());
	it("requires a task before attaching an internal material using the current draft version", async () => {
		const { updateProjectDraft } = await import("@/lib/project-draft-api");
		vi.mocked(updateProjectDraft)
			.mockReset()
			.mockResolvedValue({ ...materialDraft, version: 2 });
		renderMaterialDraft();
		const attach = screen.getByRole("button", {
			name: "关联到当前草稿: 可读路线图",
		});
		expect(attach).toBeDisabled();
		fireEvent.click(attach);
		expect(updateProjectDraft).not.toHaveBeenCalled();
		fireEvent.click(
			screen.getByRole("checkbox", {
				name: "资料任务关联: 可读路线图: 准备报告",
			}),
		);
		fireEvent.click(attach);
		await waitFor(() =>
			expect(updateProjectDraft).toHaveBeenCalledWith(
				materialDraft.id,
				1,
				expect.objectContaining({
					materials: [
						expect.objectContaining({
							id: "doc-1",
							kind: "document",
							task_ids: ["item-1"],
						}),
					],
				}),
			),
		);
	});

	it("preserves existing material purpose, required flag, and source when changing task association", async () => {
		const { updateProjectDraft } = await import("@/lib/project-draft-api");
		vi.mocked(updateProjectDraft)
			.mockReset()
			.mockResolvedValue({ ...materialDraft, version: 2 });
		const material: ProjectDraft["materials"][number] = {
			id: "doc-1",
			kind: "document",
			source_type: "internal",
			source_version: "pinned-version",
			source_location: "pinned-location",
			processed_status: "ready",
			purpose: "验收依据",
			required: true,
			task_ids: ["item-1"],
		};
		renderMaterialDraft({ ...materialDraft, materials: [material] });
		fireEvent.click(
			screen.getByRole("checkbox", { name: /可读路线图: 审核报告/ }),
		);
		fireEvent.click(
			screen.getByRole("button", { name: "关联到当前草稿: 可读路线图" }),
		);
		await waitFor(() =>
			expect(updateProjectDraft).toHaveBeenCalledWith(
				materialDraft.id,
				1,
				expect.objectContaining({
					materials: [{ ...material, task_ids: ["item-1", "item-2"] }],
				}),
			),
		);
	});

	it("does not carry unsaved material selections to another draft", async () => {
		renderMaterialDraft(
			materialDraft,
			[],
			[{ ...materialDraft, id: "another-draft" }],
		);
		fireEvent.click(
			screen.getByRole("checkbox", { name: /可读路线图: 准备报告/ }),
		);
		fireEvent.click(screen.getAllByRole("button", { name: "预览" })[1]);
		expect(
			screen.getByRole("checkbox", { name: /可读路线图: 准备报告/ }),
		).not.toBeChecked();
		expect(
			screen.getByRole("button", { name: "关联到当前草稿: 可读路线图" }),
		).toBeDisabled();
	});

	it("lists a parsed uploaded file privately until its task association is explicitly saved", async () => {
		const { updateProjectDraft, uploadProjectDraftFile } = await import(
			"@/lib/project-draft-api"
		);
		vi.mocked(updateProjectDraft)
			.mockReset()
			.mockResolvedValue({ ...materialDraft, version: 2 });
		vi.mocked(uploadProjectDraftFile).mockReset();
		renderMaterialDraft(materialDraft, [readyFile]);
		const attach = screen.getByRole("button", {
			name: "关联到当前草稿: brief.pdf",
		});
		expect(attach).toBeDisabled();
		expect(updateProjectDraft).not.toHaveBeenCalled();
		fireEvent.click(
			screen.getByRole("checkbox", {
				name: "资料任务关联: brief.pdf: 准备报告",
			}),
		);
		fireEvent.click(attach);
		await waitFor(() =>
			expect(updateProjectDraft).toHaveBeenCalledWith(
				materialDraft.id,
				1,
				expect.objectContaining({
					materials: [
						expect.objectContaining({
							id: "file-1",
							kind: "file",
							source_type: "upload",
							processed_status: "ready",
							task_ids: ["item-1"],
						}),
					],
				}),
			),
		);
		expect(uploadProjectDraftFile).not.toHaveBeenCalled();
	});

	it("refreshes a conflicted association and lets a persisted file be attached without uploading again", async () => {
		const { updateProjectDraft, uploadProjectDraftFile } = await import(
			"@/lib/project-draft-api"
		);
		vi.mocked(updateProjectDraft)
			.mockReset()
			.mockRejectedValueOnce({ response: { status: 409 } })
			.mockResolvedValueOnce({ ...materialDraft, version: 3 });
		vi.mocked(uploadProjectDraftFile).mockReset();
		const qc = renderMaterialDraft(materialDraft, [readyFile]);
		const invalidate = vi
			.spyOn(qc, "invalidateQueries")
			.mockImplementation(async () => {
				qc.setQueryData(
					["private-project-draft-preview", materialDraft.id, 2],
					{
						...materialDraft,
						version: 2,
						can_publish: false,
						issues: [],
						employee_views: [],
					},
				);
				qc.setQueryData(
					["private-project-drafts"],
					[{ ...materialDraft, version: 2 }],
				);
			});
		fireEvent.click(
			screen.getByRole("checkbox", { name: /brief.pdf: 准备报告/ }),
		);
		fireEvent.click(
			screen.getByRole("button", { name: "关联到当前草稿: brief.pdf" }),
		);
		expect(
			await screen.findByText("资料关联保存失败，请刷新后重试。"),
		).toBeInTheDocument();
		await waitFor(() =>
			expect(
				screen.getByRole("checkbox", { name: /brief.pdf: 准备报告/ }),
			).not.toBeChecked(),
		);
		expect(invalidate).toHaveBeenCalledWith({
			queryKey: ["private-project-drafts"],
		});
		fireEvent.click(
			screen.getByRole("checkbox", { name: /brief.pdf: 准备报告/ }),
		);
		fireEvent.click(
			screen.getByRole("button", { name: "关联到当前草稿: brief.pdf" }),
		);
		await waitFor(() =>
			expect(updateProjectDraft).toHaveBeenLastCalledWith(
				materialDraft.id,
				2,
				expect.anything(),
			),
		);
		expect(uploadProjectDraftFile).not.toHaveBeenCalled();
	});

	it("does not automatically share a successful upload", async () => {
		const { updateProjectDraft, uploadProjectDraftFile } = await import(
			"@/lib/project-draft-api"
		);
		vi.mocked(updateProjectDraft).mockReset();
		vi.mocked(uploadProjectDraftFile).mockReset().mockResolvedValue(readyFile);
		const qc = renderMaterialDraft();
		vi.spyOn(qc, "invalidateQueries").mockImplementation(async () => {
			qc.setQueryData(
				["private-project-draft-files", materialDraft.id],
				[readyFile],
			);
		});
		fireEvent.change(screen.getByLabelText("上传文件"), {
			target: {
				files: [
					new File(["pdf bytes"], "brief.pdf", { type: "application/pdf" }),
				],
			},
		});
		expect(
			await screen.findByRole("button", { name: "关联到当前草稿: brief.pdf" }),
		).toBeDisabled();
		expect(updateProjectDraft).not.toHaveBeenCalled();
		expect(
			screen.queryByRole("button", { name: "重试上传" }),
		).not.toBeInTheDocument();
	});

	it("records an explicitly selected public result with its URL and verification metadata", async () => {
		const { updateProjectDraft } = await import("@/lib/project-draft-api");
		vi.mocked(updateProjectDraft)
			.mockReset()
			.mockResolvedValue({ ...materialDraft, version: 2 });
		const qc = renderMaterialDraft();
		qc.setQueryData(
			["private-project-draft-web-search", "public"],
			[
				{
					title: "Public source",
					url: "https://example.com/source",
					snippet: "Public result",
					source_type: "web",
					accessed_at: "2026-09-24T08:00:00Z",
					read_status: "unverified",
					processed_status: "unverified",
				},
			],
		);
		fireEvent.click(screen.getByRole("button", { name: "搜索公开网页" }));
		fireEvent.change(screen.getByRole("textbox", { name: "搜索公开网页" }), {
			target: { value: "public" },
		});
		fireEvent.click(screen.getByRole("button", { name: "搜索公开网页" }));
		expect(await screen.findByText("Public source")).toBeInTheDocument();
		const attach = screen.getByRole("button", {
			name: "关联到当前草稿: Public source",
		});
		expect(attach).toBeDisabled();
		fireEvent.click(
			screen.getByRole("checkbox", {
				name: "资料任务关联: Public source: 准备报告",
			}),
		);
		fireEvent.click(
			screen.getByRole("button", {
				name: "关联到当前草稿: Public source",
			}),
		);
		await waitFor(() =>
			expect(updateProjectDraft).toHaveBeenCalledWith(
				materialDraft.id,
				1,
				expect.objectContaining({
					materials: [
						expect.objectContaining({
							id: expect.stringMatching(
								/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/,
							),
							kind: "document",
							source_type: "web",
							source_location: "https://example.com/source",
							source_version: "2026-09-24T08:00:00Z",
							accessed_at: "2026-09-24T08:00:00Z",
							read_status: "unverified",
							processed_status: "unverified",
							purpose: "Public result",
							task_ids: ["item-1"],
						}),
					],
				}),
			),
		);
	});
});

describe("PrivateProjectDraftsCard", () => {
	beforeEach(() => vi.clearAllMocks());
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

	it("keeps a failed file available for manual retry", async () => {
		const { uploadProjectDraftFile } = await import("@/lib/project-draft-api");
		vi.mocked(uploadProjectDraftFile)
			.mockRejectedValueOnce(new Error("upload failed"))
			.mockResolvedValueOnce({} as never);
		renderCard([
			{
				id: "draft-upload",
				name: "上传重试",
				version: 1,
				state: "editable",
			},
		]);
		const file = new File(["hello"], "notes.txt", { type: "text/plain" });
		fireEvent.change(screen.getByLabelText("上传文件"), {
			target: { files: [file] },
		});

		expect(
			await screen.findByText("上传或解析失败，请重试。"),
		).toBeInTheDocument();
		fireEvent.click(screen.getByRole("button", { name: "重试上传" }));
		await waitFor(() =>
			expect(uploadProjectDraftFile).toHaveBeenCalledTimes(2),
		);
	});

	it("offers persisted failed files for retry after refresh", async () => {
		const { uploadProjectDraftFile } = await import("@/lib/project-draft-api");
		vi.mocked(uploadProjectDraftFile).mockResolvedValue({} as never);
		const queryClient = new QueryClient({
			defaultOptions: { queries: { retry: false, staleTime: Infinity } },
		});
		queryClient.setQueryData(
			["private-project-drafts"],
			[
				{
					id: "draft-persisted",
					name: "持久化重试",
					version: 1,
					state: "editable",
				},
			],
		);
		queryClient.setQueryData(
			["private-project-draft-preview", "draft-persisted", 1],
			{
				id: "draft-persisted",
				version: 1,
				can_publish: false,
				issues: [{ field: "items", message: "至少添加一项任务" }],
			},
		);
		queryClient.setQueryData(
			["private-project-draft-files", "draft-persisted"],
			[
				{
					file_id: "file-failed",
					file_name: "failed.pdf",
					upload_status: "uploaded",
					processing_status: "failed",
				},
			],
		);
		render(
			<QueryClientProvider client={queryClient}>
				<PrivateProjectDraftsCard />
			</QueryClientProvider>,
		);
		fireEvent.click(screen.getByRole("button", { name: "预览" }));
		const file = new File(["replacement"], "replacement.pdf", {
			type: "application/pdf",
		});
		fireEvent.change(await screen.findByLabelText("failed.pdf: 重试上传"), {
			target: { files: [file] },
		});
		await waitFor(() =>
			expect(uploadProjectDraftFile).toHaveBeenCalledWith(
				"draft-persisted",
				file,
				"file-failed",
			),
		);
	});

	it("switches the preview to a selected employee view", async () => {
		const queryClient = new QueryClient({
			defaultOptions: { queries: { retry: false, staleTime: Infinity } },
		});
		queryClient.setQueryData(
			["private-project-drafts"],
			[{ id: "draft-scope", name: "按人预览", version: 1, state: "editable" }],
		);
		queryClient.setQueryData(
			["private-project-draft-preview", "draft-scope", 1],
			{
				id: "draft-scope",
				version: 1,
				name: "按人预览",
				goal: "目标",
				items: [],
				participants: [{ user_id: "user-a" }, { user_id: "user-b" }],
				materials: [],
				can_publish: true,
				issues: [],
				employee_views: [
					{
						user_id: "user-a",
						items: [{ id: "item-a", title: "甲任务" }],
						materials: [
							{ id: "material-a", kind: "document", required: false },
						],
					},
					{
						user_id: "user-b",
						items: [{ id: "item-b", title: "乙任务" }],
						materials: [
							{ id: "material-b", kind: "document", required: false },
						],
					},
				],
			},
		);
		render(
			<QueryClientProvider client={queryClient}>
				<PrivateProjectDraftsCard />
			</QueryClientProvider>,
		);
		fireEvent.click(screen.getByRole("button", { name: "预览" }));
		const selector = await screen.findByRole("combobox", { name: "参与人" });
		expect(screen.getByText("甲任务")).toBeInTheDocument();
		fireEvent.change(selector, { target: { value: "user-b" } });
		expect(screen.getByText("乙任务")).toBeInTheDocument();
		expect(screen.queryByText("甲任务")).not.toBeInTheDocument();
		expect(screen.getByText(/资料: 1/)).toBeInTheDocument();
	});

	it("keeps the employee view available while preview issues are shown", async () => {
		const queryClient = new QueryClient({
			defaultOptions: { queries: { retry: false, staleTime: Infinity } },
		});
		queryClient.setQueryData(
			["private-project-drafts"],
			[{ id: "draft-issues", name: "待补充", version: 1, state: "editable" }],
		);
		queryClient.setQueryData(
			["private-project-draft-preview", "draft-issues", 1],
			{
				id: "draft-issues",
				version: 1,
				name: "待补充",
				goal: "目标",
				items: [],
				participants: [{ user_id: "user-a" }],
				materials: [],
				can_publish: false,
				issues: [{ field: "items", message: "缺少任务" }],
				employee_views: [
					{
						user_id: "user-a",
						items: [{ id: "item-a", title: "待补充任务" }],
						materials: [],
					},
				],
			},
		);
		render(
			<QueryClientProvider client={queryClient}>
				<PrivateProjectDraftsCard />
			</QueryClientProvider>,
		);
		fireEvent.click(screen.getByRole("button", { name: "预览" }));
		expect(
			await screen.findByRole("combobox", { name: "参与人" }),
		).toBeInTheDocument();
		expect(screen.getByText("待补充任务")).toBeInTheDocument();
	});
});
