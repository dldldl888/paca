import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { ClipboardList, MessageSquare, Target } from "lucide-react";
import { useTranslation } from "react-i18next";

import { PrivateProjectDraftsCard } from "@/components/home/private-project-drafts-card";
import { usePermissions } from "@/hooks/use-permissions";
import { useProjectPermissions } from "@/hooks/use-project-permissions";
import { type Project, projectsQueryOptions } from "@/lib/project-api";

function ManagerProjectLinks({
	project,
	globalManage,
}: {
	project: Project;
	globalManage: boolean;
}) {
	const { t } = useTranslation("shared");
	const { hasProjectPermission } = useProjectPermissions(project.id);
	if (!globalManage && !hasProjectPermission("tasks.manage")) return null;

	return (
		<section
			aria-label={`${t("home.manager.title")} · ${project.name}`}
			className="rounded-lg border border-border/60 bg-card p-3"
		>
			<p className="mb-2 text-sm font-medium">{project.name}</p>
			<div className="grid gap-2 sm:grid-cols-3">
				<Link
					to="/projects/$projectId/drafts"
					params={{ projectId: project.id }}
					className="flex items-center gap-2 rounded-md border border-border/50 px-3 py-2 text-sm hover:border-primary/40 hover:text-primary"
				>
					<Target className="size-4" />
					{t("home.manager.goals", { project: project.name })}
				</Link>
				<Link
					to="/projects/$projectId/interactions/backlog"
					params={{ projectId: project.id }}
					className="flex items-center gap-2 rounded-md border border-border/50 px-3 py-2 text-sm hover:border-primary/40 hover:text-primary"
				>
					<ClipboardList className="size-4" />
					{t("home.manager.tasks", { project: project.name })}
				</Link>
				<Link
					to="/projects/$projectId/interactions/backlog"
					params={{ projectId: project.id }}
					className="flex items-center gap-2 rounded-md border border-border/50 px-3 py-2 text-sm hover:border-primary/40 hover:text-primary"
				>
					<MessageSquare className="size-4" />
					{t("home.manager.feedback", { project: project.name })}
				</Link>
			</div>
		</section>
	);
}

export function ManagerWorkspaceEntry() {
	const { hasPermission } = usePermissions();
	const { data } = useQuery(projectsQueryOptions());
	const globalManage = hasPermission("tasks.manage");
	const canCreateProjects = hasPermission("projects.create");
	const projects = data?.items ?? [];

	if (projects.length === 0 && !canCreateProjects) return null;

	return (
		<div className="space-y-2 px-6 pb-6">
			{canCreateProjects && <PrivateProjectDraftsCard projects={projects} />}
			{projects.map((project) => (
				<ManagerProjectLinks
					key={project.id}
					project={project}
					globalManage={globalManage}
				/>
			))}
		</div>
	);
}
