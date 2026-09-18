import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
	act,
	fireEvent,
	render,
	screen,
	waitFor,
	within,
} from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { AxiosError, type InternalAxiosRequestConfig } from "axios";
import i18next from "i18next";
import { useState } from "react";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { loadLanguage } from "@/i18n/config";
import { apiClient } from "@/lib/api-client";
import type { Draft, DraftItem } from "@/lib/draft-api";
import { DraftWorkspace } from "./draft-workspace";

const projectId = "10000000-0000-4000-8000-000000000001";
const draftId = "20000000-0000-4000-8000-000000000001";
const itemId = "30000000-0000-4000-8000-000000000001";
const memberId = "40000000-0000-4000-8000-000000000001";
const item: DraftItem = {
	id: itemId,
	title: "准备报告",
	requirements: "整理数据",
	criteria: "交付可复核表格",
	assignee_member_id: memberId,
	due_date: "2026-09-30T23:59:00+08:00",
	start_date: "",
	importance: 2,
	dependencies: [],
};
const original: Draft = {
	id: draftId,
	project_id: projectId,
	owner_user_id: "owner",
	version: 1,
	state: "editable",
	goal: "私人经营目标",
	items: [item],
	sources: ["私密资料"],
	assumptions: ["隐藏假设"],
	published_tasks: [],
	created_at: "2026-09-17T00:00:00Z",
	updated_at: "2026-09-17T00:00:00Z",
};
const generation: NonNullable<Draft["generation"]> = {
	id: "50000000-0000-4000-8000-000000000001",
	version: 2,
	agent_id: "hermes",
	status: "running",
	conversation_id: "conversation-1",
	chat_session_id: "session-1",
};
const member = {
	id: memberId,
	project_id: projectId,
	user_id: "employee",
	project_role_id: "role",
	username: "alice",
	full_name: "员工甲",
	role_name: "员工",
	member_type: "human",
};
const hermes = {
	id: "hermes",
	name: "公司助手 Hermes",
	handle: "hermes",
	agent_scope: "global",
	agent_type: "acp",
	acp_provider: "custom",
	llm_provider: "",
	llm_model: "",
	llm_base_url: "",
	has_acp_bridge_token: true,
	has_mcp_api_key: false,
	system_prompt: "",
	git_committer_name: "",
	git_committer_email: "",
	docker_enabled: false,
	parallelism_limit: 1,
	created_at: original.created_at,
	updated_at: original.updated_at,
};
type Request = { method: string; url: string; body: Record<string, unknown> };
let requests: Request[];
let server: Draft;
let permissions: Record<string, boolean>;
let rejectSave: boolean;
let failPublish: boolean;
let losePublishResponse: boolean;
let issues: { field: string; message: string }[];
let projectHasHermes: boolean;
let releaseImport: (() => void) | undefined;
let delayImport: boolean;
let delayCreate: boolean;
let releaseCreate: (() => void) | undefined;
const originalAdapter = apiClient.instance.defaults.adapter;
const clients: QueryClient[] = [];

beforeEach(async () => {
	await loadLanguage("zh-CN");
	await i18next.changeLanguage("zh-CN");
	requests = [];
	server = structuredClone(original);
	permissions = { "tasks.write": true, "tasks.manage": true };
	rejectSave = false;
	failPublish = false;
	losePublishResponse = false;
	issues = [];
	projectHasHermes = true;
	delayImport = false;
	delayCreate = false;
	releaseCreate = undefined;
	releaseImport = undefined;
	apiClient.instance.defaults.adapter = async (
		config: InternalAxiosRequestConfig,
	) => {
		const url = config.url ?? "";
		const body = config.data ? JSON.parse(config.data) : {};
		requests.push({ method: config.method ?? "get", url, body });
		const ok = (data: unknown) => ({
			data: { success: true, data },
			status: 200,
			statusText: "OK",
			headers: {},
			config,
		});
		const fail = (code: string, status: number) => {
			throw new AxiosError(code, code, config, undefined, {
				data: { success: false, error_code: code, error: "请求失败" },
				status,
				statusText: "Error",
				headers: {},
				config,
			});
		};
		if (url.endsWith("/members/me/permissions")) return ok({ permissions });
		if (url.endsWith("/members"))
			return ok([
				member,
				{
					...member,
					id: "robot",
					member_type: "agent",
					full_name: "自动智能体",
				},
			]);
		if (url === "/agents") return ok({ items: [hermes] });
		if (url.endsWith("/agents"))
			return ok({ items: projectHasHermes ? [hermes] : [] });
		if (url.endsWith("/conversations/conversation-1"))
			return ok({
				id: "conversation-1",
				agent_id: "hermes",
				project_id: projectId,
				trigger_type: "chat_message",
				chat_session_id: "session-1",
				status: "finished",
				iteration_count: 1,
				input_tokens: 0,
				output_tokens: 0,
				total_tokens: 0,
				created_at: original.created_at,
				updated_at: original.updated_at,
			});
		if (url.endsWith("/drafts") && config.method === "get")
			return ok({ items: [server] });
		if (url.endsWith("/drafts") && config.method === "post") {
			if (delayCreate)
				await new Promise<void>((resolve) => {
					releaseCreate = resolve;
				});
			server = { ...server, ...body };
			return ok(server);
		}
		if (url.endsWith(`/drafts/${draftId}`) && config.method === "get")
			return ok(server);
		if (url.endsWith(`/drafts/${draftId}`) && config.method === "patch") {
			if (rejectSave) return fail("DRAFT_CONFLICT", 409);
			server = { ...server, ...body, version: server.version + 1 };
			return ok(server);
		}
		if (url.endsWith("/preview"))
			return ok({
				id: draftId,
				project_id: projectId,
				version: server.version,
				items: [{ ...item, title: "服务器员工标题" }],
				issues,
				can_publish: issues.length === 0,
				goal: "不应泄露的额外字段",
			});
		if (url.endsWith("/generate")) {
			server = { ...server, version: 2, generation };
			return ok(server);
		}
		if (url.endsWith("/import")) {
			if (delayImport)
				await new Promise<void>((resolve) => {
					releaseImport = resolve;
				});
			server = {
				...server,
				version: 3,
				items: [{ ...item, title: "Hermes生成标题" }],
				generation: { ...generation, status: "imported" },
			};
			return ok(server);
		}
		if (url.endsWith("/publish")) {
			if (failPublish) return fail("INTERNAL_ERROR", 500);
			if (issues.length > 0) return fail("BAD_REQUEST", 400);
			server = {
				...server,
				state: "published",
				published_tasks: [
					{ item_id: itemId, task_id: "task-1", task_number: 42 },
				],
			};
			if (losePublishResponse)
				throw new AxiosError("Connection lost", "ERR_NETWORK", config);
			return ok(server);
		}
		throw new Error(`Unexpected request: ${config.method} ${url}`);
	};
});

afterEach(() => {
	apiClient.instance.defaults.adapter = originalAdapter;
	for (const client of clients) client.clear();
	clients.length = 0;
});

function mount(selected: string | undefined = draftId) {
	const client = new QueryClient({
		defaultOptions: {
			queries: { retry: false, staleTime: Infinity },
			mutations: { retry: false },
		},
	});
	clients.push(client);
	function Harness() {
		const [id, setId] = useState<string | undefined>(selected);
		return (
			<DraftWorkspace
				key={id ?? "new"}
				projectId={projectId}
				draftId={id}
				onSelectDraft={setId}
			/>
		);
	}
	return render(
		<QueryClientProvider client={client}>
			<Harness />
		</QueryClientProvider>,
	);
}

const publishedRequests = () =>
	requests.filter((r) => r.url.endsWith("/publish"));

describe("DraftWorkspace", () => {
	it("does not offer a global Hermes that is not a current project member", async () => {
		projectHasHermes = false;
		mount();
		await screen.findByDisplayValue("准备报告");
		expect(screen.getByRole("button", { name: "生成任务草稿" })).toBeDisabled();
		expect(
			screen.getByRole("option", { name: "暂无可用小派" }),
		).toBeInTheDocument();
	});

	it("resolves a pending confirmed publication with its original id when current preview validation fails", async () => {
		failPublish = true;
		const view = mount();
		await screen.findByDisplayValue("准备报告");
		await userEvent.click(screen.getByRole("button", { name: "员工预览" }));
		await userEvent.click(
			await screen.findByRole("button", { name: "确认并分派" }),
		);
		await screen.findByRole("alert");
		const approved = publishedRequests()[0].body;
		view.unmount();
		failPublish = false;
		issues = [
			{ field: "items[0].assignee_member_id", message: "负责人已离开项目" },
		];
		mount();
		await screen.findByDisplayValue("准备报告");
		await userEvent.click(screen.getByRole("button", { name: "员工预览" }));
		await screen.findByText("负责人已离开项目");
		expect(
			screen.queryByRole("button", { name: "确认并分派" }),
		).not.toBeInTheDocument();
		await userEvent.click(
			screen.getByRole("button", { name: "重试已确认的分派" }),
		);
		await waitFor(() => expect(screen.getByLabelText("标题 1")).toBeEnabled());
		expect(publishedRequests()[1].body).toEqual(approved);
		fireEvent.change(screen.getByLabelText("标题 1"), {
			target: { value: "现在可修正" },
		});
		expect(screen.getByRole("button", { name: "保存草稿" })).toBeEnabled();
	});
	it("locks editing during the first save so a delayed create keeps the new draft address", async () => {
		delayCreate = true;
		mount("");
		const goal = await screen.findByLabelText("工作目标（私人）");
		fireEvent.change(goal, { target: { value: "首次保存目标" } });
		await userEvent.click(screen.getByRole("button", { name: "保存草稿" }));
		await waitFor(() => expect(releaseCreate).toBeDefined());
		await userEvent.type(goal, "等待时新增");
		expect(goal).toHaveValue("首次保存目标");
		await act(async () => {
			releaseCreate?.();
		});
		expect(screen.getByLabelText("工作目标（私人）")).toHaveValue(
			"首次保存目标",
		);
		expect(screen.getByLabelText("工作目标（私人）")).toBeEnabled();
		expect(screen.getByRole("button", { name: "员工预览" })).toBeEnabled();
		expect(screen.queryByRole("alert")).not.toBeInTheDocument();
		expect(screen.getByLabelText("我的草稿")).toHaveValue(draftId);
	});
	it("creates an incomplete draft without publishing and shows server issues before confirmation", async () => {
		mount("");
		const goal = await screen.findByLabelText("工作目标（私人）");
		fireEvent.change(goal, { target: { value: "先保存不完整目标" } });
		expect(screen.getByRole("button", { name: "生成任务草稿" })).toBeDisabled();
		await userEvent.click(screen.getByRole("button", { name: "保存草稿" }));
		await waitFor(() =>
			expect(screen.getByRole("button", { name: "员工预览" })).toBeEnabled(),
		);
		expect(
			requests.find((r) => r.url.endsWith("/drafts") && r.method === "post")
				?.body,
		).toEqual({
			goal: "先保存不完整目标",
			items: [],
			sources: [],
			assumptions: [],
		});
		issues = [{ field: "items", message: "请至少添加一个任务项" }];
		await userEvent.click(screen.getByRole("button", { name: "员工预览" }));
		expect(await screen.findByText("请至少添加一个任务项")).toBeInTheDocument();
		expect(
			screen.queryByRole("button", { name: "确认并分派" }),
		).not.toBeInTheDocument();
		expect(publishedRequests()).toHaveLength(0);
	});

	it("shows the committed task after a lost publish response and refresh without sending a second batch", async () => {
		losePublishResponse = true;
		const view = mount();
		await screen.findByDisplayValue("准备报告");
		await userEvent.click(screen.getByRole("button", { name: "员工预览" }));
		await userEvent.click(
			await screen.findByRole("button", { name: "确认并分派" }),
		);
		await screen.findByRole("alert");
		view.unmount();
		mount();
		expect(await screen.findByRole("link", { name: /#42/ })).toHaveAttribute(
			"href",
			`/projects/${projectId}/tasks/task-1`,
		);
		expect(publishedRequests()).toHaveLength(1);
		expect(
			screen.queryByRole("button", { name: "确认并分派" }),
		).not.toBeInTheDocument();
	});
	it("keeps generation private and imports only the server-linked conversation", async () => {
		mount();
		await screen.findByDisplayValue("私人经营目标");
		await userEvent.click(screen.getByRole("button", { name: "生成任务草稿" }));
		expect(
			await screen.findByRole("link", { name: "查看原始私人对话" }),
		).toHaveAttribute(
			"href",
			`/projects/${projectId}/conversations/conversation-1`,
		);
		expect(publishedRequests()).toHaveLength(0);
		await userEvent.click(
			await screen.findByRole("button", { name: "导入生成结果" }),
		);
		await screen.findByDisplayValue("Hermes生成标题");
		expect(requests.find((r) => r.url.endsWith("/generate"))?.body).toEqual({
			expected_version: 1,
			agent_id: "hermes",
		});
		expect(requests.find((r) => r.url.endsWith("/import"))?.body).toEqual({
			expected_version: 2,
			generation_id: generation.id,
		});
		expect(publishedRequests()).toHaveLength(0);
	});

	it("renders only server preview fields and invalidates confirmation on any edit", async () => {
		mount();
		await screen.findByDisplayValue("准备报告");
		await userEvent.click(screen.getByRole("button", { name: "员工预览" }));
		const preview = await screen.findByRole("region", {
			name: "员工实际可见内容",
		});
		expect(within(preview).getByText("服务器员工标题")).toBeInTheDocument();
		for (const text of [
			"私人经营目标",
			"私密资料",
			"隐藏假设",
			"不应泄露的额外字段",
		])
			expect(preview).not.toHaveTextContent(text);
		expect(screen.getByRole("button", { name: "确认并分派" })).toBeEnabled();
		fireEvent.change(screen.getByLabelText("工作目标（私人）"), {
			target: { value: "已编辑目标" },
		});
		expect(
			screen.queryByRole("button", { name: "确认并分派" }),
		).not.toBeInTheDocument();
		expect(screen.getByRole("button", { name: "员工预览" })).toBeDisabled();
		expect(screen.getByText(/请先保存/)).toBeInTheDocument();
	});

	it("retains a stable publication id through failure, remount and rapid retry, then shows task links", async () => {
		failPublish = true;
		const view = mount();
		await screen.findByDisplayValue("准备报告");
		await userEvent.click(screen.getByRole("button", { name: "员工预览" }));
		const confirm = await screen.findByRole("button", { name: "确认并分派" });
		fireEvent.click(confirm);
		fireEvent.click(confirm);
		await screen.findByRole("alert");
		expect(screen.getByLabelText("标题 1")).toBeDisabled();
		expect(publishedRequests()).toHaveLength(1);
		const first = publishedRequests()[0].body;
		expect(first).toEqual({
			expected_version: 1,
			publication_id: expect.stringMatching(/^[\da-f-]{36}$/),
			confirmed: true,
		});
		view.unmount();
		failPublish = false;
		mount();
		await screen.findByDisplayValue("准备报告");
		await waitFor(() => expect(screen.getByLabelText("标题 1")).toBeDisabled());
		await userEvent.click(screen.getByRole("button", { name: "员工预览" }));
		await userEvent.click(
			await screen.findByRole("button", { name: "确认并分派" }),
		);
		expect(await screen.findByRole("link", { name: /#42/ })).toHaveAttribute(
			"href",
			`/projects/${projectId}/tasks/task-1`,
		);
		expect(publishedRequests()).toHaveLength(2);
		expect(publishedRequests()[1].body).toEqual(first);
		expect(
			screen.queryByRole("button", { name: "确认并分派" }),
		).not.toBeInTheDocument();
	});

	it("preserves unsaved edits after a version conflict and exposes an explicit reload", async () => {
		mount();
		await screen.findByDisplayValue("准备报告");
		fireEvent.change(screen.getByLabelText("标题 1"), {
			target: { value: "我的未保存标题" },
		});
		rejectSave = true;
		await userEvent.click(screen.getByRole("button", { name: "保存草稿" }));
		expect(await screen.findByRole("alert")).toHaveTextContent(/已变化/);
		expect(screen.getByDisplayValue("我的未保存标题")).toBeInTheDocument();
		server = {
			...server,
			version: 2,
			items: [{ ...item, title: "其他页面已保存标题" }],
		};
		await userEvent.click(
			screen.getByRole("button", { name: "重新加载已保存内容" }),
		);
		await screen.findByDisplayValue("其他页面已保存标题");
		expect(
			(screen.getByLabelText("重新加载前的未保存内容") as HTMLTextAreaElement)
				.value,
		).toContain("我的未保存标题");
	});

	it("does not overwrite edits made while a generation import request is in flight", async () => {
		server = { ...server, version: 2, generation };
		delayImport = true;
		mount();
		await screen.findByDisplayValue("准备报告");
		await userEvent.click(
			await screen.findByRole("button", { name: "导入生成结果" }),
		);
		await waitFor(() => expect(releaseImport).toBeDefined());
		fireEvent.change(screen.getByLabelText("标题 1"), {
			target: { value: "等待期间手工修改" },
		});
		await act(async () => {
			releaseImport?.();
		});
		expect(screen.getByDisplayValue("等待期间手工修改")).toBeInTheDocument();
		expect(
			screen.queryByDisplayValue("Hermes生成标题"),
		).not.toBeInTheDocument();
		expect(await screen.findByRole("alert")).toHaveTextContent(/已变化/);
	});

	it("saves stable item ids, explicit Shanghai date-only deadlines and human assignees", async () => {
		mount();
		await screen.findByDisplayValue("准备报告");
		expect(screen.getByText(/Asia\/Shanghai/)).toBeInTheDocument();
		expect(
			screen.queryByRole("option", { name: "自动智能体" }),
		).not.toBeInTheDocument();
		await userEvent.click(screen.getByRole("button", { name: "添加任务项" }));
		fireEvent.change(screen.getByLabelText("标题 2"), {
			target: { value: "第二项" },
		});
		fireEvent.change(screen.getByLabelText("截止日期 2"), {
			target: { value: "2026-10-01" },
		});
		await userEvent.click(screen.getByRole("button", { name: "保存草稿" }));
		await waitFor(() =>
			expect(screen.getByRole("button", { name: "保存草稿" })).toBeDisabled(),
		);
		const saved = requests.find((r) => r.method === "patch")?.body;
		expect(saved?.items).toEqual([
			item,
			expect.objectContaining({
				id: expect.stringMatching(/^[\da-f-]{36}$/),
				title: "第二项",
				due_date: "2026-10-01T23:59:00+08:00",
			}),
		]);
		expect(saved).not.toHaveProperty("owner_user_id");
	});

	it("does not request drafts or expose editing for an employee without management rights", async () => {
		permissions = { "tasks.write": true };
		mount();
		expect(
			await screen.findByText("仅项目任务管理者可使用工作目标与草稿。"),
		).toBeInTheDocument();
		expect(requests.some((r) => r.url.includes("/drafts"))).toBe(false);
		expect(
			screen.queryByRole("button", { name: "保存草稿" }),
		).not.toBeInTheDocument();
	});
});
