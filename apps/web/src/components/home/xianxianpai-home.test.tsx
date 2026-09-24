import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, within } from "@testing-library/react";
import i18next from "i18next";
import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { loadLanguage } from "@/i18n/config";

const mocks = vi.hoisted(() => ({
	organizationNavigation: vi.fn(() => (
		<div data-testid="organization-navigation" />
	)),
	notificationBell: vi.fn(() => <button type="button">通知铃</button>),
	notificationsQueryFn: vi.fn(async () => ({
		items: [],
		unread_count: 0,
		next_cursor: null,
	})),
	newConversation: vi.fn(() => <div data-testid="new-conversation" />),
}));

vi.mock("@/components/company/organization-navigation", () => ({
	OrganizationNavigation: mocks.organizationNavigation,
}));
vi.mock("@/components/app-shell/notification-bell", () => ({
	NotificationBell: mocks.notificationBell,
}));
vi.mock("@/components/projects/agents/new-conversation-thread", () => ({
	NewConversationThread: mocks.newConversation,
}));
vi.mock("@/lib/auth-api", () => ({
	currentUserQueryOptions: {
		queryKey: ["auth", "me"],
		queryFn: async () => null,
	},
}));
vi.mock("@/lib/project-api", async () => ({
	...(await vi.importActual<typeof import("@/lib/project-api")>(
		"@/lib/project-api",
	)),
	projectsQueryOptions: () => ({
		queryKey: ["projects"],
		queryFn: async () => ({ items: [], total: 0 }),
	}),
	taskStatusesQueryOptions: (id: string) => ({
		queryKey: ["statuses", id],
		queryFn: async () => [],
	}),
	taskTypesQueryOptions: (id: string) => ({
		queryKey: ["types", id],
		queryFn: async () => [],
	}),
	projectMembersQueryOptions: (id: string) => ({
		queryKey: ["members", id],
		queryFn: async () => [],
	}),
}));
vi.mock("@/lib/interaction-api", async () => ({
	...(await vi.importActual<typeof import("@/lib/interaction-api")>(
		"@/lib/interaction-api",
	)),
	assignedTasksQueryOptions: () => ({
		queryKey: ["assigned-tasks"],
		queryFn: async () => ({ items: [], next_cursor: null }),
		initialPageParam: undefined,
		getNextPageParam: () => undefined,
	}),
}));
vi.mock("@/lib/doc-api", () => ({
	docListQueryOptions: (projectId: string) => ({
		queryKey: ["docs", projectId],
		queryFn: async () => ({ items: [] }),
	}),
}));
vi.mock("@/lib/agent-api", () => ({
	chattableAgentsQueryOptions: {
		queryKey: ["global-agents", "chattable"],
		queryFn: async () => [],
	},
}));
vi.mock("@/components/projects/agents/agent-picker", () => ({
	isCompanyAssistant: (agent: { handle?: string; name?: string }) =>
		agent.handle === "hermes" && agent.name === "公司助手 Hermes",
}));
vi.mock("@/lib/notification-api", () => ({
	notificationsQueryOptions: {
		queryKey: ["notifications"],
		initialPageParam: undefined,
		getNextPageParam: (page: { next_cursor?: string | null }) =>
			page.next_cursor ?? undefined,
		queryFn: mocks.notificationsQueryFn,
	},
}));
vi.mock("@tanstack/react-router", () => ({
	useNavigate: () => vi.fn(),
	Link: ({
		children,
		to,
		params,
	}: {
		children: ReactNode;
		to: string;
		params?: Record<string, string>;
	}) => (
		<a
			href={
				params
					? to
							.replace("$projectId", params.projectId ?? "")
							.replace("$docId", params.docId ?? "")
					: to
			}
		>
			{children}
		</a>
	),
}));
beforeEach(async () => {
	mocks.notificationsQueryFn.mockReset();
	mocks.notificationsQueryFn.mockResolvedValue({
		items: [],
		unread_count: 0,
		next_cursor: null,
	});
	await loadLanguage("zh-CN");
	await i18next.changeLanguage("zh-CN");
});

import { XianxianpaiHome } from "./xianxianpai-home";

function renderHome(component: ReactNode, cache: Record<string, unknown> = {}) {
	const queryClient = new QueryClient({
		defaultOptions: { queries: { retry: false, staleTime: Infinity } },
	});
	for (const [key, value] of Object.entries(cache))
		queryClient.setQueryData(key.split(","), value);
	const view = render(
		<QueryClientProvider client={queryClient}>{component}</QueryClientProvider>,
	);
	return { ...view, queryClient };
}

describe("XianxianpaiHome", () => {
	it("renders employee work entry points in the promised order", async () => {
		renderHome(<XianxianpaiHome />, {
			"auth,me": { full_name: "员工甲" },
			"assigned-tasks": {
				pages: [
					{
						items: [
							{
								id: "task-1",
								project_id: "engineering",
								title: "提交本周工作反馈",
								task_number: 42,
								importance: 0,
								custom_fields: {},
								created_at: "2026-09-18T00:00:00Z",
								updated_at: "2026-09-18T00:00:00Z",
							},
						],
					},
				],
				pageParams: [undefined],
			},
			projects: { items: [{ id: "company", name: "公司资料" }], total: 1 },
			"docs,company": [{ id: "doc-1", title: "员工手册" }],
			"global-agents,chattable": [
				{ id: "xiaopai", handle: "hermes", name: "公司助手 Hermes" },
			],
			notifications: {
				pages: [{ items: [{ id: "notification-1" }], unread_count: 2 }],
				pageParams: [undefined],
			},
		});

		const main = screen.getByRole("region", { name: "芯鲜派员工工作台" });
		for (const label of [
			"欢迎回来，员工甲",
			"我的待办",
			"通知",
			"小派",
			"公司资料",
		]) {
			expect(within(main).getByText(label)).toBeInTheDocument();
		}
		expect(main.textContent?.indexOf("我的待办")).toBeLessThan(
			main.textContent?.indexOf("通知") ?? 0,
		);
		expect(main.textContent?.indexOf("通知")).toBeLessThan(
			main.textContent?.indexOf("小派") ?? 0,
		);
		expect(main.textContent?.indexOf("小派")).toBeLessThan(
			main.textContent?.indexOf("公司资料") ?? 0,
		);
		expect(within(main).getByRole("link", { name: /^小派/ })).toHaveAttribute(
			"href",
			"/conversations",
		);
		expect(
			within(main).getByRole("link", { name: /^公司资料/ }),
		).toHaveAttribute("href", "/projects/company/docs/doc-1");
		expect(
			await within(main).findByText("提交本周工作反馈"),
		).toBeInTheDocument();
		expect(within(main).getByText("2 条未读通知")).toBeInTheDocument();
		expect(
			within(main).getByTestId("organization-navigation"),
		).toBeInTheDocument();
	});

	it("does not treat an unrelated agent as Xiaopai", async () => {
		renderHome(<XianxianpaiHome />, {
			projects: { items: [], total: 0 },
			"global-agents,chattable": [
				{ id: "other", handle: "other", name: "Other" },
			],
			notifications: {
				pages: [{ items: [], unread_count: 0 }],
				pageParams: [undefined],
			},
		});

		expect(
			await screen.findByText("小派当前不可用，请稍后再试。"),
		).toBeInTheDocument();
	});

	it("renders Chinese empty and offline guidance without a developer error", async () => {
		renderHome(<XianxianpaiHome />, {
			"auth,me": undefined,
			projects: { items: [], total: 0 },
			notifications: { pages: [], pageParams: [] },
			"global-agents,chattable": [],
		});

		expect(screen.getByText("欢迎来到芯鲜派")).toBeInTheDocument();
		expect(
			await screen.findByText("小派当前不可用，请稍后再试。"),
		).toBeInTheDocument();
		expect(screen.getByText("暂无新通知")).toBeInTheDocument();
		expect(
			await screen.findByText("目前没有分配给你的未完成任务。"),
		).toBeInTheDocument();
		expect(
			screen.getByText("公司资料暂不可用，请联系负责人。"),
		).toBeInTheDocument();
		expect(
			screen.queryByText(/TypeError|undefined|stack/i),
		).not.toBeInTheDocument();
	});

	it("keeps project and notification loading states explicit", () => {
		renderHome(<XianxianpaiHome />);

		expect(screen.getByText("公司资料加载中")).toBeInTheDocument();
		expect(screen.getByText("通知加载中")).toBeInTheDocument();
		expect(screen.getByText("小派连接中")).toBeInTheDocument();
	});

	it("keeps cached notifications visible when a background refresh fails", async () => {
		const { queryClient } = renderHome(<XianxianpaiHome />, {
			"auth,me": { full_name: "员工甲" },
			projects: { items: [], total: 0 },
			notifications: {
				pages: [
					{
						items: [{ id: "notification-1" }],
						unread_count: 2,
					},
				],
				pageParams: [undefined],
			},
			"global-agents,chattable": [],
		});
		mocks.notificationsQueryFn.mockRejectedValueOnce(new Error("offline"));

		await queryClient.invalidateQueries({ queryKey: ["notifications"] });

		expect(mocks.notificationsQueryFn).toHaveBeenCalled();
		expect(queryClient.getQueryState(["notifications"])?.status).toBe("error");
		expect(await screen.findByText("2 条未读通知")).toBeInTheDocument();
		expect(
			screen.queryByText("通知暂时无法加载，请稍后再试。"),
		).not.toBeInTheDocument();
	});
});
