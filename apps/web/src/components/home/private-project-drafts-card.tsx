import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { FilePenLine } from "lucide-react";
import { useTranslation } from "react-i18next";

import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { usePermissions } from "@/hooks/use-permissions";
import {
	type ProjectDraft,
	projectDraftsQueryOptions,
} from "@/lib/project-draft-api";

function statusKey(draft: ProjectDraft) {
	switch (draft.generation?.status) {
		case "starting":
		case "running":
			return "generating" as const;
		case "failed":
			return "failed" as const;
		case "imported":
			return "imported" as const;
		default:
			return "editable" as const;
	}
}

export function PrivateProjectDraftsCard() {
	const { t } = useTranslation("shared");
	const { hasPermission } = usePermissions();
	const allowed = hasPermission("projects.create");
	const drafts = useQuery({ ...projectDraftsQueryOptions, enabled: allowed });

	if (!allowed) return null;

	return (
		<Card
			aria-label={t("home.privateDrafts.title")}
			role="region"
			className="border-border/60"
		>
			<CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
				<CardTitle className="flex items-center gap-2 text-base">
					<FilePenLine className="size-4 text-primary" />
					{t("home.privateDrafts.title")}
				</CardTitle>
				<Link
					to="/conversations"
					className="text-sm text-primary underline-offset-4 hover:underline"
				>
					{t("home.privateDrafts.openChat")}
				</Link>
			</CardHeader>
			<CardContent className="pt-0">
				{drafts.isPending ? (
					<p className="text-sm text-muted-foreground">
						{t("home.privateDrafts.loading")}
					</p>
				) : drafts.isError ? (
					<p className="text-sm text-muted-foreground">
						{t("home.privateDrafts.error")}
					</p>
				) : drafts.data.length === 0 ? (
					<p className="text-sm text-muted-foreground">
						{t("home.privateDrafts.empty")}
					</p>
				) : (
					<ul className="grid gap-2 sm:grid-cols-2">
						{drafts.data.map((draft) => (
							<li
								key={draft.id}
								className="flex min-w-0 items-center justify-between gap-3 rounded-md border border-border/50 px-3 py-2"
							>
								<div className="min-w-0">
									<p className="truncate text-sm font-medium">{draft.name}</p>
									<p className="text-xs text-muted-foreground">
										{t("home.privateDrafts.version", {
											version: draft.version,
										})}
									</p>
								</div>
								<Badge variant="outline" className="shrink-0">
									{t(`home.privateDrafts.status.${statusKey(draft)}`)}
								</Badge>
							</li>
						))}
					</ul>
				)}
			</CardContent>
		</Card>
	);
}
