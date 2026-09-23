import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import i18next from "i18next";
import type { ComponentProps } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { SidebarProvider, SidebarTrigger } from "@/components/ui/sidebar";
import { loadLanguage } from "@/i18n/config";
import { currentUserQueryOptions, type User } from "@/lib/auth-api";
import { projectsQueryOptions } from "@/lib/project-api";
import { AppSidebar, canShowProjectNavItem } from "./app-sidebar";
import { BrandingEffects } from "./branding-effects";

const state = vi.hoisted(() => ({ mobile: false, admin: false }));
vi.mock("@/hooks/use-mobile", () => ({ useIsMobile: () => state.mobile }));
vi.mock("@/hooks/use-branding", () => ({ useBranding: () => undefined }));
vi.mock("@/hooks/use-theme-mode", () => ({
	useThemeMode: () => ({ mode: "light", resolvedMode: "light", set: vi.fn() }),
}));
vi.mock("@/hooks/use-permissions", () => ({
	usePermissions: () => ({ hasPermission: () => state.admin }),
}));
vi.mock("@/lib/plugins/registry", () => ({
	usePluginRegistry: () => ({ getNavItems: () => [] }),
}));
vi.mock("@/lib/plugins/extension-point", () => ({
	ExtensionPoint: () => null,
}));
vi.mock("@tanstack/react-router", () => ({
	Link: ({
		to,
		params: _params,
		...props
	}: ComponentProps<"a"> & { to: string; params?: unknown }) => (
		<a href={to} {...props} />
	),
	useParams: () => ({}),
	useNavigate: () => vi.fn(),
	useRouterState: ({
		select,
	}: {
		select: (state: { location: { pathname: string } }) => string;
	}) => select({ location: { pathname: "/home" } }),
}));
function wrapper({ children }: { children: React.ReactNode }) {
	const client = new QueryClient({
		defaultOptions: { queries: { retry: false, staleTime: Infinity } },
	});
	const user: User = {
		id: "employee",
		full_name: "员工甲",
		username: "employee",
		role: "user",
		must_change_password: false,
		created_at: "2026-09-18T00:00:00Z",
	};
	client.setQueryData<User | null>(currentUserQueryOptions.queryKey, user);
	client.setQueryData(projectsQueryOptions().queryKey, {
		items: [],
		total: 0,
		page: 1,
		page_size: 20,
	});
	return (
		<QueryClientProvider client={client}>
			<SidebarProvider>{children}</SidebarProvider>
		</QueryClientProvider>
	);
}
beforeEach(async () => {
	state.admin = false;
	state.mobile = false;
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
});
describe("employee sidebar", () => {
	it("renders Xianxianpai branding and document metadata", () => {
		document.title = "Paca";
		document.head.innerHTML =
			'<link id="app-favicon" rel="icon" href="/old.png">';
		render(
			<>
				<BrandingEffects />
				<AppSidebar />
			</>,
			{ wrapper },
		);
		expect(screen.getByRole("img", { name: "芯鲜派" })).toHaveAttribute(
			"src",
			"/brand/xianxianpai-logo.png",
		);
		expect(screen.getByRole("link", { name: "芯鲜派" })).toHaveAttribute(
			"href",
			"/home",
		);
		expect(document.title).toBe("芯鲜派");
		expect(document.querySelector('link[rel="icon"]')).toHaveAttribute(
			"href",
			"/brand/xianxianpai-logo.png",
		);
	});
	it("renders the mobile Xianxianpai brand as a navigable link", async () => {
		state.mobile = true;
		render(
			<>
				<SidebarTrigger />
				<AppSidebar />
			</>,
			{ wrapper },
		);
		await userEvent.click(screen.getByRole("button", { name: /侧边栏/ }));
		expect(await screen.findByRole("dialog")).toBeInTheDocument();
		expect(screen.getByRole("link", { name: "芯鲜派" })).toHaveAttribute(
			"href",
			"/home",
		);
		expect(screen.getByRole("img", { name: "芯鲜派" })).toHaveAttribute(
			"src",
			"/brand/xianxianpai-logo.png",
		);
	});
	it("hides platform-only entries from employees and opens the user menu", async () => {
		render(<AppSidebar />, { wrapper });
		expect(screen.getByRole("link", { name: /首页/ })).toHaveAttribute(
			"href",
			"/home",
		);
		expect(screen.getByRole("link", { name: /对话/ })).toHaveAttribute(
			"href",
			"/conversations",
		);
		await userEvent.click(screen.getByRole("button", { name: /员工甲/ }));
		expect(
			await screen.findByRole("menuitem", { name: /个人资料/ }),
		).toBeInTheDocument();
		expect(
			screen.queryByRole("menuitem", { name: /API 密钥/ }),
		).not.toBeInTheDocument();
		expect(
			screen.queryByText(/开发者|桥接|插件管理|环境管理|智能体管理/),
		).not.toBeInTheDocument();
		expect(
			screen.queryByRole("link", { name: /系统设置/ }),
		).not.toBeInTheDocument();
	});
	it("keeps API key navigation available to a system administrator", async () => {
		state.admin = true;
		render(<AppSidebar />, { wrapper });
		await userEvent.click(screen.getByRole("button", { name: /员工甲/ }));
		expect(
			await screen.findByRole("menuitem", { name: /API 密钥/ }),
		).toBeInTheDocument();
		state.admin = false;
	});
});
describe("project management navigation", () => {
	it("hides technical management pages for an ordinary member", () => {
		const access = {
			canManageDrafts: false,
			canManageProject: false,
			canManageAgents: true,
			canManageEnvironments: true,
			canManageWorkflows: true,
		};
		expect(canShowProjectNavItem("agents", access)).toBe(false);
		expect(canShowProjectNavItem("environments", access)).toBe(false);
		expect(canShowProjectNavItem("automation", access)).toBe(false);
	});
});
