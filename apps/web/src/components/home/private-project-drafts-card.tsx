import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { FilePenLine } from "lucide-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";

import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { usePermissions } from "@/hooks/use-permissions";
import {
	type ProjectDraft,
	projectDraftFilesQueryOptions,
	projectDraftMaterialsQueryOptions,
	projectDraftPreviewQueryOptions,
	projectDraftsQueryOptions,
	projectDraftWebSearchQueryOptions,
	publishProjectDraft,
	uploadProjectDraftFile,
} from "@/lib/project-draft-api";

function publicationStorageKey(draftId: string, version: number) {
	return `private-project-draft-publication:${draftId}:${version}`;
}

function stablePublicationID(draft: ProjectDraft) {
	if (draft.publication_id) return draft.publication_id;
	const key = publicationStorageKey(draft.id, draft.version);
	const stored = window.localStorage.getItem(key);
	if (stored) return stored;
	const created = crypto.randomUUID();
	window.localStorage.setItem(key, created);
	return created;
}

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
	const qc = useQueryClient();
	const allowed = hasPermission("projects.create");
	const drafts = useQuery({ ...projectDraftsQueryOptions, enabled: allowed });
	const [materialsOpen, setMaterialsOpen] = useState(false);
	const [search, setSearch] = useState("");
	const [submittedSearch, setSubmittedSearch] = useState("");
	const [webSearchOpen, setWebSearchOpen] = useState(false);
	const [webSearch, setWebSearch] = useState("");
	const [submittedWebSearch, setSubmittedWebSearch] = useState("");
	const [previewDraftId, setPreviewDraftId] = useState<string>();
	const [uploadingDraftId, setUploadingDraftId] = useState<string>();
	const [failedUploads, setFailedUploads] = useState<Record<string, File>>({});
	const [uploadError, setUploadError] = useState("");
	const [publishedDraftId, setPublishedDraftId] = useState<string>();
	const [publishedProjectIDs, setPublishedProjectIDs] = useState<
		Record<string, string>
	>({});
	const [publicationPendingDraftId, setPublicationPendingDraftId] =
		useState<string>();
	const [publishError, setPublishError] = useState("");
	const materials = useQuery({
		...projectDraftMaterialsQueryOptions(submittedSearch),
		enabled: allowed && materialsOpen,
	});
	const webResults = useQuery({
		...projectDraftWebSearchQueryOptions(submittedWebSearch),
		enabled: allowed && webSearchOpen && submittedWebSearch.length > 0,
	});
	const visibleDrafts = chatSessionId
		? (drafts.data ?? []).filter(
				(draft) => draft.chat_session_id === chatSessionId,
			)
		: (drafts.data ?? []);
	const previewDraft = visibleDrafts.find(
		(draft) => draft.id === previewDraftId,
	);
	const preview = useQuery({
		...projectDraftPreviewQueryOptions(
			previewDraftId ?? "",
			previewDraft?.version ?? 0,
		),
		enabled: allowed && !!previewDraft,
	});
	async function uploadDraftFile(draftId: string, file: File) {
		setUploadError("");
		setUploadingDraftId(draftId);
		setFailedUploads((current) => {
			const next = { ...current };
			delete next[draftId];
			return next;
		});
		try {
			await uploadProjectDraftFile(draftId, file);
			await qc.invalidateQueries({
				queryKey: projectDraftFilesQueryOptions(draftId).queryKey,
			});
		} catch {
			setUploadError(t("home.privateDrafts.uploadError"));
			setFailedUploads((current) => ({ ...current, [draftId]: file }));
		} finally {
			setUploadingDraftId(undefined);
		}
	}
	const publish = useMutation({
		mutationFn: async (draft: ProjectDraft) =>
			publishProjectDraft(draft.id, draft.version, stablePublicationID(draft)),
		onMutate: (draft) => {
			setPublishError("");
			setPublicationPendingDraftId(draft.id);
		},
		onSuccess: (result, draft) => {
			setPublishedDraftId(draft.id);
			setPublishedProjectIDs((current) => ({
				...current,
				[draft.id]: result.project_id,
			}));
			setPublicationPendingDraftId(undefined);
			void qc.invalidateQueries({
				queryKey: projectDraftsQueryOptions.queryKey,
			});
			void qc.invalidateQueries({
				queryKey: projectDraftPreviewQueryOptions(draft.id, draft.version)
					.queryKey,
			});
		},
		onError: (error) => {
			setPublicationPendingDraftId(undefined);
			const status = (error as { response?: { status?: number } })?.response
				?.status;
			setPublishError(
				status == null || status >= 500
					? t("home.privateDrafts.publicationPending")
					: t("home.privateDrafts.publishConflict"),
			);
			// A lost response may still have committed. Re-read the private list
			// so a refresh/retry can recover the stable publication result.
			void qc.invalidateQueries({
				queryKey: projectDraftsQueryOptions.queryKey,
			});
		},
	});

	if (!allowed) return null;
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
									{(draft.published_project_id ||
										publishedProjectIDs[draft.id]) && (
										<Link
											to="/projects/$projectId"
											params={{
												projectId:
													draft.published_project_id ||
													publishedProjectIDs[draft.id] ||
													"",
											}}
											className="mt-1 block text-xs text-primary underline-offset-4 hover:underline"
										>
											{t("home.privateDrafts.viewProject")}
										</Link>
									)}
									<label className="mt-2 inline-flex cursor-pointer text-xs text-primary underline-offset-4 hover:underline">
										{uploadingDraftId === draft.id
											? t("home.privateDrafts.uploading")
											: t("home.privateDrafts.upload")}
										<input
											type="file"
											accept=".txt,.md,.pdf,.docx,text/plain,text/markdown,application/pdf,application/vnd.openxmlformats-officedocument.wordprocessingml.document"
											className="sr-only"
											disabled={!!uploadingDraftId}
											onChange={(event) => {
												const file = event.target.files?.[0];
												if (!file) return;
												const input = event.currentTarget;
												void uploadDraftFile(draft.id, file).finally(() => {
													input.value = "";
												});
											}}
										/>
									</label>
									{failedUploads[draft.id] && (
										<button
											type="button"
											className="mt-2 ml-3 text-xs text-primary underline-offset-4 hover:underline"
											disabled={!!uploadingDraftId}
											onClick={() =>
												void uploadDraftFile(draft.id, failedUploads[draft.id])
											}
										>
											{t("home.privateDrafts.retryUpload")}
										</button>
									)}
									<button
										type="button"
										className="mt-2 ml-3 text-xs text-primary underline-offset-4 hover:underline"
										onClick={() =>
											setPreviewDraftId((current) =>
												current === draft.id ? undefined : draft.id,
											)
										}
									>
										{t("home.privateDrafts.preview")}
									</button>
								</div>
								<Badge variant="outline" className="shrink-0">
									{draft.published_project_id || publishedDraftId === draft.id
										? t("home.privateDrafts.published")
										: t(`home.privateDrafts.status.${statusKey(draft)}`)}
								</Badge>
							</li>
						))}
					</ul>
				)}
				{previewDraft && (
					<div className="mt-3 rounded-md border border-border/50 px-3 py-2 text-sm">
						{preview.isPending ? (
							<p className="text-muted-foreground">
								{t("home.privateDrafts.materialLoading")}
							</p>
						) : preview.isError ? (
							<p className="text-destructive">
								{t("home.privateDrafts.materialError")}
							</p>
						) : preview.data?.can_publish ? (
							<div className="space-y-2">
								<p className="text-green-600">
									{publishedDraftId === previewDraft.id ||
									previewDraft.published_project_id
										? t("home.privateDrafts.published")
										: t("home.privateDrafts.previewReady")}
								</p>
								<div className="space-y-1 text-xs text-muted-foreground">
									<p className="font-medium text-foreground">
										{preview.data.name}
									</p>
									<p>{preview.data.goal}</p>
									{preview.data.manager_user_id && (
										<p>
											{t("home.privateDrafts.previewManager")}:{" "}
											{preview.data.manager_user_id}
										</p>
									)}
									<p>
										{t("home.privateDrafts.previewItems")}:{" "}
										{preview.data.items.length}
									</p>
									<ul className="list-disc pl-4">
										{preview.data.items.map((item) => (
											<li key={item.id}>{item.title}</li>
										))}
									</ul>
									<p>
										{t("home.privateDrafts.previewParticipants")}:{" "}
										{preview.data.participants.length}
									</p>
									<p>
										{t("home.privateDrafts.previewMaterials")}:{" "}
										{preview.data.materials.length}
									</p>
								</div>
								{publishedDraftId !== previewDraft.id &&
									!previewDraft.published_project_id && (
										<button
											type="button"
											className="rounded-md border px-3 py-1.5 text-sm"
											disabled={publish.isPending}
											onClick={() => publish.mutate(previewDraft)}
										>
											{publish.isPending &&
											publicationPendingDraftId === previewDraft.id
												? t("home.privateDrafts.publishing")
												: t("home.privateDrafts.publish")}
										</button>
									)}
							</div>
						) : (
							<>
								<p className="text-destructive">
									{t("home.privateDrafts.previewIssues")}
								</p>
								<ul className="mt-1 list-disc pl-4 text-xs text-muted-foreground">
									{preview.data?.issues.map((issue) => (
										<li key={`${issue.field}:${issue.message}`}>
											{issue.message}
										</li>
									))}
								</ul>
							</>
						)}
					</div>
				)}
				{uploadError && (
					<p className="mt-2 text-xs text-destructive">{uploadError}</p>
				)}
				{publishError && (
					<p className="mt-2 text-xs text-destructive">{publishError}</p>
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
				<div className="mt-3 border-t border-border/50 pt-3">
					<button
						type="button"
						className="text-sm text-primary underline-offset-4 hover:underline"
						onClick={() => setWebSearchOpen((open) => !open)}
					>
						{t(
							webSearchOpen
								? "home.privateDrafts.hideWebSearch"
								: "home.privateDrafts.showWebSearch",
						)}
					</button>
					{webSearchOpen && (
						<div className="mt-3 space-y-2">
							<form
								className="flex gap-2"
								onSubmit={(event) => {
									event.preventDefault();
									setSubmittedWebSearch(webSearch.trim());
								}}
							>
								<input
									className="h-9 min-w-0 flex-1 rounded-md border bg-background px-2 text-sm"
									value={webSearch}
									placeholder={t("home.privateDrafts.webSearchPlaceholder")}
									aria-label={t("home.privateDrafts.webSearch")}
									onChange={(event) => setWebSearch(event.target.value)}
								/>
								<button
									type="submit"
									className="rounded-md border px-3 text-sm"
								>
									{t("home.privateDrafts.webSearch")}
								</button>
							</form>
							{webResults.isPending ? (
								<p className="text-sm text-muted-foreground">
									{t("home.privateDrafts.webSearchLoading")}
								</p>
							) : webResults.isError ? (
								<p className="text-sm text-muted-foreground">
									{t("home.privateDrafts.webSearchError")}
								</p>
							) : webResults.data?.length ? (
								<ul className="space-y-2 text-sm">
									{webResults.data.map((result) => (
										<li
											key={result.url}
											className="rounded-md border border-border/40 px-2 py-1.5"
										>
											<a
												href={result.url}
												target="_blank"
												rel="noreferrer"
												className="font-medium text-primary underline-offset-4 hover:underline"
											>
												{result.title || result.url}
											</a>
											{result.snippet && (
												<p className="mt-1 text-xs text-muted-foreground">
													{result.snippet}
												</p>
											)}
											<p className="mt-1 text-xs text-muted-foreground">
												{result.read_status} · {result.accessed_at}
											</p>
										</li>
									))}
								</ul>
							) : (
								<p className="text-sm text-muted-foreground">
									{t("home.privateDrafts.webSearchEmpty")}
								</p>
							)}
						</div>
					)}
				</div>
			</CardContent>
		</Card>
	);
}
