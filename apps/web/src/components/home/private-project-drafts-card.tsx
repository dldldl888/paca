import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { FilePenLine } from "lucide-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";

import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { usePermissions } from "@/hooks/use-permissions";
import {
	type ProjectDraft,
	projectDraftMaterialsQueryOptions,
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

export function PrivateProjectDraftsCard({
	chatSessionId,
}: {
	chatSessionId?: string;
} = {}) {
	const { t } = useTranslation("shared");
	const { hasPermission } = usePermissions();
	const allowed = hasPermission("projects.create");
	const drafts = useQuery({ ...projectDraftsQueryOptions, enabled: allowed });
	const [materialsOpen, setMaterialsOpen] = useState(false);
	const [search, setSearch] = useState("");
	const [submittedSearch, setSubmittedSearch] = useState("");
	const materials = useQuery({
		...projectDraftMaterialsQueryOptions(submittedSearch),
		enabled: allowed && materialsOpen,
	});

	if (!allowed) return null;
	const visibleDrafts = chatSessionId
		? (drafts.data ?? []).filter(
				(draft) => draft.chat_session_id === chatSessionId,
			)
		: drafts.data;
	if (
		!drafts.isPending &&
		!drafts.isError &&
		chatSessionId &&
		visibleDrafts.length === 0
	) {
		return null;
	}

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
					{!chatSessionId && t("home.privateDrafts.openChat")}
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
				) : visibleDrafts.length === 0 ? (
					<p className="text-sm text-muted-foreground">
						{t("home.privateDrafts.empty")}
					</p>
				) : (
					<ul className="grid gap-2 sm:grid-cols-2">
						{visibleDrafts.map((draft) => (
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
				<div className="mt-4 border-t border-border/50 pt-3">
					<button
						type="button"
						className="text-sm text-primary underline-offset-4 hover:underline"
						onClick={() => setMaterialsOpen((open) => !open)}
					>
						{t(
							materialsOpen
								? "home.privateDrafts.hideMaterials"
								: "home.privateDrafts.showMaterials",
						)}
					</button>
					{materialsOpen && (
						<div className="mt-3 space-y-2">
							<form
								className="flex gap-2"
								onSubmit={(event) => {
									event.preventDefault();
									setSubmittedSearch(search.trim());
								}}
							>
								<input
									className="h-9 min-w-0 flex-1 rounded-md border bg-background px-2 text-sm"
									value={search}
									placeholder={t(
										"home.privateDrafts.materialSearchPlaceholder",
									)}
									aria-label={t("home.privateDrafts.materialSearch")}
									onChange={(event) => setSearch(event.target.value)}
								/>
								<button
									type="submit"
									className="rounded-md border px-3 text-sm"
								>
									{t("home.privateDrafts.search")}
								</button>
							</form>
							{materials.isPending ? (
								<p className="text-sm text-muted-foreground">
									{t("home.privateDrafts.materialLoading")}
								</p>
							) : materials.isError ? (
								<p className="text-sm text-muted-foreground">
									{t("home.privateDrafts.materialError")}
								</p>
							) : materials.data?.length ? (
								<ul className="space-y-1 text-sm">
									{materials.data.map((material) => (
										<li
											key={material.id}
											className="flex items-center justify-between gap-2 rounded-md border border-border/40 px-2 py-1.5"
										>
											<span className="truncate">{material.title}</span>
											<span className="shrink-0 text-xs text-muted-foreground">
												{material.processed_status}
											</span>
										</li>
									))}
								</ul>
							) : (
								<p className="text-sm text-muted-foreground">
									{t("home.privateDrafts.materialEmpty")}
								</p>
							)}
						</div>
					)}
				</div>
			</CardContent>
		</Card>
	);
}
