import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import i18next from "i18next";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { loadLanguage } from "@/i18n/config";
import { currentUserQueryOptions } from "@/lib/auth-api";
import type { Task } from "@/lib/interaction-api";
import type { TaskStatus } from "@/lib/project-api";
import { TaskDetailModal } from "./index";

const { get } = vi.hoisted(() => ({ get: vi.fn() }));
vi.mock("@/lib/api-client", () => ({ apiClient: { instance: { get } } }));
vi.mock("@tanstack/react-router", () => ({ useNavigate: () => vi.fn() }));
vi.mock("@/lib/plugins/extension-point", () => ({
	ExtensionPoint: () => null,
}));
// BlockNote editing is covered in browser; keep the task/activity components real.
vi.mock("./description-section", () => ({
	DescriptionSection: ({ description }: { description: unknown }) => (
		<section aria-label="任务要求">{JSON.stringify(description)}</section>
	),
}));
vi.mock("@/components/shared/comment-blocknote", () => ({
	CommentEditor: () => <textarea aria-label="反馈" />,
	CommentDisplay: ({ blocks }: { blocks: unknown }) => (
		<p>{JSON.stringify(blocks)}</p>
	),
	blocksToText: (blocks: unknown) => JSON.stringify(blocks),
	textToBlocks: (text: string) => [
		{ type: "paragraph", content: [{ type: "text", text }] },
	],
}));
vi.mock("@/components/shared/content-diff", () => ({
	ContentDiffDialog: () => null,
}));
vi.mock("./attachments-section", () => ({ AttachmentsSection: () => null }));
vi.mock("./task-links-section", () => ({ TaskLinksSection: () => null }));
const task: Task = {
	id: "assigned-task",
	project_id: "engineering",
	title: "完成客户回访",
	task_number: 42,
	status_id: "done",
	task_type_id: null,
	sprint_id: null,
	parent_task_id: null,
	description: [
		{
			type: "paragraph",
			content: [
				{ type: "text", text: "要求：回访三位客户；标准：提交反馈报告" },
			],
		},
	],
	importance: 0,
	assignee_ids: ["employee-member"],
	reporter_id: "manager-member",
	custom_fields: {},
	view_position: null,
	view_group_key: null,
	created_at: "2026-09-18T00:00:00Z",
	updated_at: "2026-09-18T01:00:00Z",
};
const statuses: TaskStatus[] = [
	{
		id: "done",
		name: "已完成",
		category: "done",
		color: "#00aa00",
		project_id: "engineering",
		position: 1,
		created_at: "2026-09-18T00:00:00Z",
		updated_at: "2026-09-18T00:00:00Z",
	},
];
function clientFor(user: string) {
	const client = new QueryClient({
		defaultOptions: { queries: { retry: false, staleTime: Infinity } },
	});
	client.setQueryData(currentUserQueryOptions.queryKey, {
		id: user,
		username: user,
		full_name: user,
		role: "USER",
		must_change_password: false,
		created_at: "2026-09-18T00:00:00Z",
	});
	return client;
}
beforeEach(async () => {
	await loadLanguage("zh-CN");
	await i18next.changeLanguage("zh-CN");
	vi.stubGlobal(
		"ResizeObserver",
		class {
			observe() {}
			unobserve() {}
			disconnect() {}
		},
	);
	Object.defineProperty(HTMLElement.prototype, "getAnimations", {
		configurable: true,
		value: () => [],
	});
});
describe("TaskDetailModal", () => {
	it("reloads completed task and employee feedback for the manager without private context", async () => {
		const persistedFeedback = {
			id: "feedback",
			actor_id: "employee-member",
			actor_name: "员工甲",
			actor_username: "employee",
			activity_type: "comment",
			content: { text: "已完成三位客户回访，反馈报告已提交。" },
			created_at: "2026-09-18T01:00:00Z",
			updated_at: "2026-09-18T01:00:00Z",
		};
		get.mockImplementation(async (path: string) => {
			const data = path.endsWith("/activities")
				? { items: [persistedFeedback] }
				: path.endsWith("/tasks/assigned-task")
					? { ...task, private_goal: "PRIVATE_MANAGER_GOAL_DO_NOT_PUBLISH" }
					: path === "/projects/engineering"
						? { id: "engineering", name: "工程部", task_id_prefix: "XXP" }
						: path.endsWith("/members")
							? [
									{
										id: "employee-member",
										user_id: "employee",
										full_name: "员工甲",
									},
									{
										id: "manager-member",
										user_id: "manager",
										full_name: "负责人",
									},
								]
							: path.endsWith("/task-statuses")
								? { items: statuses }
								: { items: [] };
			return { data: { success: true, data } };
		});
		const employeeClient = clientFor("employee");
		const employeeView = render(
			<QueryClientProvider client={employeeClient}>
				<TaskDetailModal
					task={{ ...task, status_id: null }}
					projectId="engineering"
					open
					onOpenChange={() => {}}
					statuses={statuses}
					taskTypes={[]}
					mode="page"
				/>
			</QueryClientProvider>,
		);
		expect(
			await screen.findByText(/已完成三位客户回访，反馈报告已提交/),
		).toBeInTheDocument();
		expect(screen.getAllByText("已完成").length).toBeGreaterThan(0);
		expect(screen.getByText("完成客户回访")).toBeInTheDocument();
		expect(
			screen.queryByText(/PRIVATE_MANAGER_GOAL_DO_NOT_PUBLISH/),
		).not.toBeInTheDocument();
		employeeView.unmount();
		employeeClient.clear();
		// A fresh client forces a refetch; no employee query cache survives the role change.
		const managerClient = clientFor("manager");
		render(
			<QueryClientProvider client={managerClient}>
				<TaskDetailModal
					task={task}
					projectId="engineering"
					open
					onOpenChange={() => {}}
					statuses={statuses}
					taskTypes={[]}
					mode="page"
					canEdit={false}
				/>
			</QueryClientProvider>,
		);
		expect(
			await screen.findByText(/已完成三位客户回访，反馈报告已提交/),
		).toBeInTheDocument();
		expect(screen.getAllByText("已完成").length).toBeGreaterThan(0);
		expect(
			screen.queryByText(/PRIVATE_MANAGER_GOAL_DO_NOT_PUBLISH/),
		).not.toBeInTheDocument();
	});
});
