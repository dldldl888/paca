import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { describe, expect, it, vi } from "vitest";

const mocks = vi.hoisted(() => ({
	assignedTasks: vi.fn(() => <div data-testid="assigned-tasks" />),
	organizationNavigation: vi.fn(() => (
		<div data-testid="organization-navigation" />
	)),
	notificationBell: vi.fn(() => <button type="button">通知铃</button>),
	newConversation: vi.fn(() => <div data-testid="new-conversation" />),
}));

vi.mock("@/components/home/assigned-tasks-list", () => ({
	AssignedTasksList: mocks.assignedTasks,
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
vi.mock("@/lib/project-api", () => ({
	projectsQueryOptions: () => ({
		queryKey: ["projects"],
		queryFn: async () => ({ items: [], total: 0 }),
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
		queryFn: async () => ({ items: [], unread_count: 0, next_cursor: null }),
	},
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
vi.mock("react-i18next", () => ({
	useTranslation: () => ({
		t: (key: string, values?: { name?: string; count?: number }) =>
			({
				"home.workbench.title": "芯鲜派员工工作台",
				"home.workbench.welcome": `欢迎回来，${values?.name}`,
				"home.workbench.welcomeFallback": "欢迎来到芯鲜派",
				"home.workbench.subtitle": "从这里开始今天的工作。",
				"home.workbench.tasks": "我的待办",
				"home.workbench.notifications": "通知",
				"home.workbench.notificationsOffline": "通知暂时无法加载",
				"home.workbench.notificationsLoading": "通知加载中",
				"home.workbench.unreadNotifications": `${values?.count ?? 0} 条未读通知`,
				"home.workbench.notificationsEmpty": "暂无新通知",
				"home.workbench.xiaopai": "小派",
				"home.workbench.xiaopaiDescription": "询问公司资料，获得工作帮助。",
				"home.workbench.xiaopaiOffline": "小派暂时离线，请稍后再试",
				"home.workbench.xiaopaiLoading": "小派连接中",
				"home.workbench.xiaopaiUnavailable": "小派当前不可用，请稍后再试",
				"home.workbench.companyDocs": "公司资料",
				"home.workbench.companyDocsDescription": "查看已授权的公司知识。",
				"home.workbench.companyDocsEmpty": "公司资料暂不可用",
				"home.workbench.companyDocsLoading": "公司资料加载中",
				"home.workbench.companyDocsError": "公司资料暂时无法加载",
			})[key] ?? key,
	}),
}));

import { XianxianpaiHome } from "./xianxianpai-home";

function renderHome(cache: Record<string, unknown> = {}) {
	const queryClient = new QueryClient({
		defaultOptions: { queries: { retry: false } },
	});
	for (const [key, value] of Object.entries(cache))
		queryClient.setQueryData(key.split(","), value);
	return render(
		<QueryClientProvider client={queryClient}>
			<XianxianpaiHome />
		</QueryClientProvider>,
	);
}

describe("XianxianpaiHome", () => {
	it("renders employee work entry points in the promised order", () => {
		renderHome({
			"auth,me": { full_name: "员工甲" },
			projects: { items: [{ id: "company", name: "公司资料" }], total: 1 },
			"docs,company": [{ id: "doc-1", title: "员工手册" }],
			"global-agents,chattable": [
				{ id: "xiaopai", handle: "hermes", name: "公司助手 Hermes" },
			],
			notifications: {
				pages: [{ items: [], unread_count: 2 }],
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
		expect(within(main).getByTestId("assigned-tasks")).toBeInTheDocument();
		expect(
			within(main).getByTestId("organization-navigation"),
		).toBeInTheDocument();
	});

	it("does not treat an unrelated agent as Xiaopai", async () => {
		renderHome({
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
			await screen.findByText("小派当前不可用，请稍后再试"),
		).toBeInTheDocument();
	});

	it("renders Chinese empty and offline guidance without a developer error", async () => {
		renderHome({
			"auth,me": undefined,
			projects: { items: [], total: 0 },
			notifications: { pages: [], pageParams: [] },
			"global-agents,chattable": [],
		});

		expect(screen.getByText("欢迎来到芯鲜派")).toBeInTheDocument();
		expect(
			await screen.findByText("小派当前不可用，请稍后再试"),
		).toBeInTheDocument();
		expect(screen.getByText("暂无新通知")).toBeInTheDocument();
		expect(
			screen.queryByText(/TypeError|undefined|stack/i),
		).not.toBeInTheDocument();
	});

	it("keeps project and notification loading states explicit", () => {
		renderHome();

		expect(screen.getByText("公司资料加载中")).toBeInTheDocument();
		expect(screen.getByText("通知加载中")).toBeInTheDocument();
		expect(screen.getByText("小派连接中")).toBeInTheDocument();
	});
});
