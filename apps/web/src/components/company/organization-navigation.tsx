import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";

import { usePermissions } from "@/hooks/use-permissions";
import { type Project, projectsQueryOptions } from "@/lib/project-api";

const ORGANIZATION_SEED = [
	["董事会", "柯人元"],
	["总经办", "柯人元"],
	["人事行政部", "李帅"],
	["财务部", "郭长青"],
	["项目部", "陈柏霖"],
	["工程部", "程环"],
	["研发部", "张家伟"],
	["投融部", "李弘皓"],
	["AI效能部/品宣部", "张家伟"],
	["贸易部", "柯敏"],
	["采购部", "魏杜秋"],
] as const;

export interface OrganizationDepartment {
	name: string;
	leader: string;
	project?: Project;
}

function normalizeName(value: string): string {
	return value.replace(/[\s/\\_-]/g, "").toLocaleLowerCase();
}

function matchesProject(departmentName: string, projectName: string): boolean {
	const department = normalizeName(departmentName);
	const project = normalizeName(projectName);
	if (department === project) return true;
	return (
		department === normalizeName("AI效能部/品宣部") &&
		["AI效能部", "品宣部"].some((alias) => normalizeName(alias) === project)
	);
}

export function getOrganizationDepartments(
	projects: Project[] = [],
): OrganizationDepartment[] {
	return ORGANIZATION_SEED.map(([name, leader]) => ({
		name,
		leader,
		project: projects.find((project) => matchesProject(name, project.name)),
	}));
}

export function OrganizationNavigation() {
	const { data } = useQuery(projectsQueryOptions());
	const { hasPermission } = usePermissions();
	const canManage = hasPermission("tasks.manage");
	const departments = getOrganizationDepartments(data?.items ?? []);

	return (
		<section
			aria-labelledby="organization-navigation-title"
			className="px-6 py-6"
		>
			<h2 id="organization-navigation-title" className="text-lg font-semibold">
				芯鲜派集团
			</h2>
			<div
				role="tree"
				aria-label="芯鲜派集团组织导航"
				className="mt-3 grid gap-2 sm:grid-cols-2 lg:grid-cols-3"
			>
				{departments.map((department) => (
					<div
						key={department.name}
						role="treeitem"
						tabIndex={0}
						className="rounded-lg border border-border/60 p-3"
					>
						<div className="flex items-center justify-between gap-3">
							{department.project ? (
								<Link
									to="/projects/$projectId"
									params={{ projectId: department.project.id }}
									className="font-medium text-primary hover:underline"
								>
									{department.name}
								</Link>
							) : (
								<span className="font-medium">{department.name}</span>
							)}
							{department.project && canManage ? (
								<Link
									to="/projects/$projectId/drafts"
									params={{ projectId: department.project.id }}
									className="text-xs text-muted-foreground hover:text-foreground hover:underline"
								>
									管理
								</Link>
							) : null}
						</div>
						<p className="mt-1 text-sm text-muted-foreground">
							负责人：{department.leader}
						</p>
					</div>
				))}
			</div>
		</section>
	);
}
