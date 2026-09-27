import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { FileText } from "lucide-react";
import { useMemo } from "react";
import { useTranslation } from "react-i18next";
import { listDocuments } from "@/lib/doc-api";

interface TaskMaterialsSectionProps {
	projectId: string;
	taskId: string;
}

/**
 * Shows the document objects already authorized for this project member.
 * Project documents are shared with every project task; task-scoped documents
 * are included only when the current task is one of their explicit links.
 * The document list endpoint performs the content authorization server-side.
 */
export function TaskMaterialsSection({
	projectId,
	taskId,
}: TaskMaterialsSectionProps) {
	const { t } = useTranslation("projects");
	const {
		data: documents = [],
		isError,
		isPending,
		refetch,
	} = useQuery({
		queryKey: ["projects", projectId, "docs", "task", taskId],
		queryFn: () => listDocuments(projectId, undefined, undefined, taskId),
		enabled: Boolean(projectId && taskId),
	});
	const materials = useMemo(
		() =>
			documents.filter(
				(document) =>
					document.visibility === "project" ||
					document.visibility === "legacy_project" ||
					(document.visibility === "task" &&
						document.task_ids.includes(taskId)),
			),
		[documents, taskId],
	);

	return (
		<section aria-label={t("taskDetail.materials.title")} className="space-y-3">
			<h3 className="flex items-center gap-2 text-xs font-semibold uppercase tracking-[0.08em] text-muted-foreground/70">
				<span>{t("taskDetail.materials.title")}</span>
				<span className="h-px flex-1 bg-linear-to-r from-border/40 to-transparent" />
			</h3>
			{isPending ? (
				<p className="text-sm text-muted-foreground/60">
					{t("taskDetail.materials.loading")}
				</p>
			) : isError ? (
				<div className="flex items-center gap-3 text-sm text-muted-foreground/60">
					<span>{t("taskDetail.materials.error")}</span>
					<button
						type="button"
						onClick={() => void refetch()}
						className="rounded-md border border-border/50 px-2 py-1 text-xs text-foreground/70 hover:bg-muted/50"
					>
						{t("taskDetail.materials.retry")}
					</button>
				</div>
			) : materials.length === 0 ? (
				<p className="text-sm text-muted-foreground/60">
					{t("taskDetail.materials.empty")}
				</p>
			) : (
				<ul className="space-y-2">
					{materials.map((document) => (
						<li key={document.id}>
							<Link
								to="/projects/$projectId/docs/$docId"
								params={{ projectId, docId: document.id }}
								className="flex items-center gap-3 rounded-xl border border-border/20 bg-muted/15 px-4 py-3 text-sm transition-colors hover:border-border/40 hover:bg-muted/25"
							>
								<FileText
									className="size-4 shrink-0 text-primary/70"
									aria-hidden="true"
								/>
								<span className="min-w-0 truncate">{document.title}</span>
							</Link>
						</li>
					))}
				</ul>
			)}
		</section>
	);
}
