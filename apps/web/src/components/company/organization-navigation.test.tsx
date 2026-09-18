import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import type { ReactNode } from "react";
import { describe, expect, it, vi } from "vitest";

import type { Project } from "@/lib/project-api";

const mocks = vi.hoisted(() => ({
	canManage: false,
}));

vi.mock("@/hooks/use-permissions", () => ({
	usePermissions: () => ({
		hasPermission: (permission: string) =>
			permission === "tasks.manage" && mocks.canManage,
	}),
}));

vi.mock("@tanstack/react-router", () => ({
	Link: ({
		to,
		params,
		children,
	}: {
		to: string;
		params?: { projectId?: string };
		children: ReactNode;
	}) => (
		<a href={to.replace("$projectId", params?.projectId ?? "")}>{children}</a>
	),
}));

vi.mock("@/lib/project-api", async () => {
	const actual =
		await vi.importActual<typeof import("@/lib/project-api")>(
			"@/lib/project-api",
		);
	return {
		...actual,
		projectsQueryOptions: () => ({
			queryKey: ["organization-test-projects"],
			queryFn: async () => ({ items: [], total: 0, page: 1, page_size: 50 }),
		}),
	};
});

import {
	getOrganizationDepartments,
	OrganizationNavigation,
} from "./organization-navigation";

const project = (id: string, name: string): Project => ({
	id,
	name,
	description: "",
	is_public: false,
	task_id_prefix: id.toUpperCase(),
	settings: {},
	created_at: "2026-01-01T00:00:00Z",
});

function renderNavigation(projects: Project[]) {
	const queryClient = new QueryClient({
		defaultOptions: { queries: { retry: false } },
	});
	queryClient.setQueryData(["organization-test-projects"], {
		items: projects,
		total: projects.length,
		page: 1,
		page_size: 50,
	});
	return render(
		<QueryClientProvider client={queryClient}>
			<OrganizationNavigation />
		</QueryClientProvider>,
	);
}

describe("getOrganizationDepartments", () => {
	it("returns all 11 departments with the fixed leaders and no guessed members", () => {
		const departments = getOrganizationDepartments([
			project("board", "董事会"),
		]);

		expect(departments).toHaveLength(11);
		expect(departments.map(({ name }) => name)).toEqual([
			"董事会",
			"总经办",
			"人事行政部",
			"财务部",
			"项目部",
			"工程部",
			"研发部",
			"投融部",
			"AI效能部/品宣部",
			"贸易部",
			"采购部",
		]);
		expect(departments.map(({ leader }) => leader)).toEqual([
			"柯人元",
			"柯人元",
			"李帅",
			"郭长青",
			"陈柏霖",
			"程环",
			"张家伟",
			"李弘皓",
			"张家伟",
			"柯敏",
			"魏杜秋",
		]);
		expect(departments.every((department) => !("members" in department))).toBe(
			true,
		);
	});

	it("links only departments backed by API projects", () => {
		const departments = getOrganizationDepartments([
			project("board", "董事会"),
			project("engineering", "工程部"),
		]);

		expect(departments.find(({ name }) => name === "董事会")?.project?.id).toBe(
			"board",
		);
		expect(departments.find(({ name }) => name === "工程部")?.project?.id).toBe(
			"engineering",
		);
		expect(
			departments.find(({ name }) => name === "财务部")?.project,
		).toBeUndefined();
	});
});

describe("OrganizationNavigation", () => {
	it("hides management links without tasks.manage and never creates missing project links", async () => {
		mocks.canManage = false;
		renderNavigation([project("board", "董事会")]);

		expect(await screen.findByRole("tree")).toBeInTheDocument();
		expect(screen.getByText("董事会").closest("a")).toHaveAttribute(
			"href",
			"/projects/board",
		);
		expect(screen.getByText("财务部").closest("a")).toBeNull();
		expect(screen.queryByRole("link", { name: /管理/ })).toBeNull();
	});

	it("shows the manager entry only when tasks.manage is available", async () => {
		mocks.canManage = true;
		renderNavigation([project("board", "董事会")]);

		expect(await screen.findByRole("link", { name: /管理/ })).toHaveAttribute(
			"href",
			"/projects/board/drafts",
		);
	});
});
