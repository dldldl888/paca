import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { FilePenLine } from "lucide-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";

import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { usePermissions } from "@/hooks/use-permissions";
import {
	createProjectDraft,
	generateProjectDraft,
	importProjectDraft,
	type ProjectDraft,
	ProjectDraftUploadError,
	projectDraftFilesQueryOptions,
	projectDraftMaterialsQueryOptions,
	projectDraftPreviewQueryOptions,
	projectDraftsQueryOptions,
	projectDraftWebSearchQueryOptions,
	publishProjectDraft,
	updateProjectDraft,
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

function webMaterialID(url: string) {
	let hash = 2166136261;
	for (const char of url) hash = Math.imul(hash ^ char.charCodeAt(0), 16777619);
	return `00000000-0000-4000-8000-${(hash >>> 0).toString(16).padStart(12, "0")}`;
}

export function PrivateProjectDraftsCard({
	chatSessionId,
	agentId,
}: {
	chatSessionId?: string;
	agentId?: string;
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
	const [selectedEmployeeViewID, setSelectedEmployeeViewID] =
		useState<string>();
	const [materialTaskSelection, setMaterialTaskSelection] = useState<
		Record<string, string[]>
	>({});
	const [materialUpdating, setMaterialUpdating] = useState(false);
	const [materialError, setMaterialError] = useState("");
	const [uploadingDraftId, setUploadingDraftId] = useState<string>();
	const [failedUploads, setFailedUploads] = useState<
		Record<string, { file: File; fileId?: string }>
	>({});
	const [uploadError, setUploadError] = useState("");
	const [publishedDraftId, setPublishedDraftId] = useState<string>();
	const [publishedProjectIDs, setPublishedProjectIDs] = useState<
		Record<string, string>
	>({});
	const [publicationPendingDraftId, setPublicationPendingDraftId] =
		useState<string>();
	const [publishError, setPublishError] = useState("");
	const [draftName, setDraftName] = useState("");
	const [draftActionError, setDraftActionError] = useState("");
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
	const previewFiles = useQuery({
		...projectDraftFilesQueryOptions(previewDraftId ?? ""),
		enabled: allowed && !!previewDraft,
	});
	const employeeViews = preview.data?.employee_views ?? [];
	const selectedEmployeeView =
		employeeViews.find((view) => view.user_id === selectedEmployeeViewID) ??
		employeeViews[0];
	const materialCandidates = [
		...(materials.data ?? []).map((material) => ({
			...material,
			kind: "document" as const,
		})),
		...(previewFiles.data ?? [])
			.filter(
				(file) =>
					file.upload_status === "uploaded" &&
					file.processing_status === "ready",
			)
			.map((file) => ({
				id: file.file_id,
				title: file.file_name,
				kind: "file" as const,
				source_type: "upload" as const,
				source_location: file.file_name,
				source_version: file.updated_at,
				processed_status: file.processing_status,
			})),
	];
	async function uploadDraftFile(
		draftId: string,
		file: File,
		retryFileId?: string,
	) {
		setUploadError("");
		setMaterialError("");
		setUploadingDraftId(draftId);
		setFailedUploads((current) => {
			const next = { ...current };
			delete next[draftId];
			return next;
		});
		try {
			await uploadProjectDraftFile(draftId, file, retryFileId);
			await qc.invalidateQueries({
				queryKey: projectDraftFilesQueryOptions(draftId).queryKey,
			});
		} catch (error) {
			setUploadError(t("home.privateDrafts.uploadError"));
			setFailedUploads((current) => ({
				...current,
				[draftId]: {
					file,
					fileId:
						error instanceof ProjectDraftUploadError ? error.fileId : undefined,
				},
			}));
		} finally {
			setUploadingDraftId(undefined);
		}
	}
	async function createDraftFromChat() {
		if (!chatSessionId || !draftName.trim()) return;
		setDraftActionError("");
		try {
			const next = await createProjectDraft(chatSessionId, draftName.trim());
			qc.setQueryData<ProjectDraft[]>(
				projectDraftsQueryOptions.queryKey,
				(current) => [...(current ?? []), next],
			);
			setDraftName("");
		} catch {
			setDraftActionError(t("home.privateDrafts.draftActionError"));
		}
	}
	async function generateFromChat(draft: ProjectDraft) {
		if (
			!agentId ||
			draft.generation?.status === "starting" ||
			draft.generation?.status === "running"
		)
			return;
		setDraftActionError("");
		try {
			const next = await generateProjectDraft(
				draft,
				agentId,
				"请根据当前私人讨论整理并更新项目草稿；保留已有有效决定，未知信息留空并列出待补充项。",
			);
			qc.setQueryData<ProjectDraft[]>(
				projectDraftsQueryOptions.queryKey,
				(current) =>
					(current ?? []).map((item) => (item.id === next.id ? next : item)),
			);
		} catch {
			setDraftActionError(t("home.privateDrafts.draftActionError"));
		}
	}
	async function importGeneratedDraft(draft: ProjectDraft) {
		if (!draft.generation) return;
		setDraftActionError("");
		try {
			const next = await importProjectDraft(draft);
			qc.setQueryData<ProjectDraft[]>(
				projectDraftsQueryOptions.queryKey,
				(current) =>
					(current ?? []).map((item) => (item.id === next.id ? next : item)),
			);
		} catch {
			setDraftActionError(t("home.privateDrafts.draftActionError"));
		}
	}
	async function associateMaterial(
		material: {
			id: string;
			kind: "document" | "file";
			source_type?: "internal" | "upload" | "web";
			source_location?: string;
			source_version?: string;
			processed_status?: "pending" | "ready" | "failed" | "unverified";
			accessed_at?: string;
			read_status?: string;
			purpose?: string;
			title?: string;
		},
		selectionKey = material.id,
	) {
		if (!previewDraft || previewDraft.state !== "editable") return;
		const taskIDs = materialTaskSelection[selectionKey] ?? [];
		if (!taskIDs.length) {
			setMaterialError(t("home.privateDrafts.materialTaskRequired"));
			return;
		}
		setMaterialError("");
		setMaterialUpdating(true);
		const existing = (previewDraft.materials ?? []).find(
			(item) =>
				item.id === material.id ||
				(material.source_type === "web" &&
					item.source_type === "web" &&
					item.source_location === material.source_location),
		);
		const nextMaterials = (previewDraft.materials ?? []).filter(
			(item) =>
				item.id !== material.id &&
				!(
					material.source_type === "web" &&
					item.source_type === "web" &&
					item.source_location === material.source_location
				),
		);
		nextMaterials.push({
			...existing,
			id: material.id,
			kind: existing?.kind ?? material.kind,
			source_type: existing?.source_type ?? material.source_type,
			source_location: existing?.source_location ?? material.source_location,
			source_version: existing?.source_version ?? material.source_version,
			processed_status: existing?.processed_status ?? material.processed_status,
			accessed_at: existing?.accessed_at ?? material.accessed_at,
			read_status: existing?.read_status ?? material.read_status,
			purpose: existing?.purpose ?? material.purpose ?? "",
			task_ids: taskIDs,
			required: existing?.required ?? false,
		});
		try {
			const next = await updateProjectDraft(
				previewDraft.id,
				previewDraft.version,
				{
					name: previewDraft.name,
					goal: previewDraft.goal,
					manager_user_id: previewDraft.manager_user_id,
					items: previewDraft.items,
					participants: previewDraft.participants,
					materials: nextMaterials,
				},
			);
			qc.setQueryData<ProjectDraft[]>(
				projectDraftsQueryOptions.queryKey,
				(current) =>
					(current ?? []).map((draft) => (draft.id === next.id ? next : draft)),
			);
			setMaterialTaskSelection((current) => {
				const nextSelection = { ...current };
				delete nextSelection[selectionKey];
				return nextSelection;
			});
		} catch {
			setMaterialError(t("home.privateDrafts.materialAttachError"));
			setMaterialTaskSelection((current) => {
				const next = { ...current };
				delete next[selectionKey];
				return next;
			});
			await qc.invalidateQueries({
				queryKey: projectDraftsQueryOptions.queryKey,
			});
			await qc.invalidateQueries({
				queryKey: projectDraftPreviewQueryOptions(
					previewDraft.id,
					previewDraft.version,
				).queryKey,
			});
		} finally {
			setMaterialUpdating(false);
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
				{chatSessionId && visibleDrafts.length === 0 && !drafts.isPending && (
					<div className="space-y-2 rounded-md border border-border/50 p-3">
						<p className="text-sm font-medium">
							{t("home.privateDrafts.createFromChat")}
						</p>
						<div className="flex gap-2">
							<input
								className="h-9 min-w-0 flex-1 rounded-md border bg-background px-2 text-sm"
								aria-label={t("home.privateDrafts.draftNamePlaceholder")}
								placeholder={t("home.privateDrafts.draftNamePlaceholder")}
								value={draftName}
								onChange={(event) => setDraftName(event.target.value)}
							/>
							<button
								type="button"
								className="rounded-md border px-3 text-sm disabled:opacity-50"
								disabled={!draftName.trim()}
								onClick={() => void createDraftFromChat()}
							>
								{t("home.privateDrafts.createDraft")}
							</button>
						</div>
					</div>
				)}
				{draftActionError && (
					<p className="mt-2 text-xs text-destructive" role="alert">
						{draftActionError}
					</p>
				)}
				{drafts.isPending ? (
					<p className="text-sm text-muted-foreground">
						{t("home.privateDrafts.loading")}
					</p>
				) : drafts.isError && drafts.data == null ? (
					<p className="text-sm text-muted-foreground">
						{t("home.privateDrafts.error")}
					</p>
				) : visibleDrafts.length === 0 && !chatSessionId ? (
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
									{chatSessionId && draft.state === "editable" && (
										<div className="mt-2 flex flex-wrap gap-2">
											{draft.generation?.status !== "running" &&
												draft.generation?.status !== "starting" && (
													<button
														type="button"
														className="text-xs text-primary underline-offset-4 hover:underline disabled:opacity-50"
														disabled={!agentId}
														onClick={() => void generateFromChat(draft)}
													>
														{t("home.privateDrafts.organizeConversation")}
													</button>
												)}
											{draft.generation?.status === "running" && (
												<button
													type="button"
													className="text-xs text-primary underline-offset-4 hover:underline"
													onClick={() => void importGeneratedDraft(draft)}
												>
													{t("home.privateDrafts.importGeneration")}
												</button>
											)}
										</div>
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
											onClick={() => {
												const failed = failedUploads[draft.id];
												if (!failed) return;
												void uploadDraftFile(
													draft.id,
													failed.file,
													failed.fileId,
												);
											}}
										>
											{t("home.privateDrafts.retryUpload")}
										</button>
									)}
									<button
										type="button"
										className="mt-2 ml-3 text-xs text-primary underline-offset-4 hover:underline"
										onClick={() => {
											setMaterialTaskSelection({});
											setPreviewDraftId((current) =>
												current === draft.id ? undefined : draft.id,
											);
										}}
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
						{previewFiles.data
							?.filter(
								(file) =>
									file.processing_status === "failed" ||
									file.upload_status !== "uploaded",
							)
							.map((file) => (
								<label
									key={file.file_id}
									className="mb-2 block text-xs text-destructive underline-offset-4 hover:underline"
								>
									{file.file_name}: {t("home.privateDrafts.retryUpload")}
									<input
										type="file"
										accept=".txt,.md,.pdf,.docx,text/plain,text/markdown,application/pdf,application/vnd.openxmlformats-officedocument.wordprocessingml.document"
										className="sr-only"
										disabled={!!uploadingDraftId}
										onChange={(event) => {
											const selected = event.currentTarget.files?.[0];
											if (!selected) return;
											const input = event.currentTarget;
											void uploadDraftFile(
												previewDraft.id,
												selected,
												file.file_id,
											).finally(() => {
												input.value = "";
											});
										}}
									/>
								</label>
							))}
						{preview.isPending ? (
							<p className="text-muted-foreground">
								{t("home.privateDrafts.materialLoading")}
							</p>
						) : preview.isError && preview.data == null ? (
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
				{selectedEmployeeView && (
					<div className="space-y-1 rounded-md border border-border/40 p-2">
						<label className="flex items-center gap-2 text-xs text-foreground">
							<span>{t("home.privateDrafts.previewParticipants")}</span>
							<select
								aria-label={t("home.privateDrafts.previewParticipants")}
								className="min-w-0 flex-1 rounded-md border bg-background px-2 py-1"
								value={selectedEmployeeView.user_id}
								onChange={(event) =>
									setSelectedEmployeeViewID(event.target.value)
								}
							>
								{employeeViews.map((view) => (
									<option key={view.user_id} value={view.user_id}>
										{view.user_id}
									</option>
								))}
							</select>
						</label>
						<p>
							{t("home.privateDrafts.previewItems")}:{" "}
							{selectedEmployeeView.items.length}
						</p>
						<ul className="list-disc pl-4">
							{selectedEmployeeView.items.map((item) => (
								<li key={item.id}>{item.title}</li>
							))}
						</ul>
						<p>
							{t("home.privateDrafts.previewMaterials")}:{" "}
							{selectedEmployeeView.materials.length}
						</p>
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
							) : materialCandidates.length ? (
								<ul className="space-y-1 text-sm">
									{materialCandidates.map((material) => {
										const existing = (previewDraft?.materials ?? []).find(
											(item) => item.id === material.id,
										);
										const selected =
											materialTaskSelection[material.id] ??
											existing?.task_ids ??
											[];
										return (
											<li
												key={material.id}
												className="space-y-2 rounded-md border border-border/40 px-2 py-1.5"
											>
												<div className="flex items-center justify-between gap-2">
													<span className="truncate">{material.title}</span>
													<span className="shrink-0 text-xs text-muted-foreground">
														{material.processed_status}
													</span>
												</div>
												{previewDraft?.state === "editable" && (
													<fieldset className="space-y-1">
														<legend className="text-xs font-medium">
															{t("home.privateDrafts.materialTask")}
														</legend>
														{(previewDraft.items ?? []).map((item) => (
															<label
																key={item.id}
																className="flex items-center gap-2 text-xs"
															>
																<input
																	type="checkbox"
																	aria-label={`${t("home.privateDrafts.materialTask")}: ${material.title}: ${item.title}`}
																	checked={selected.includes(item.id)}
																	onChange={(event) =>
																		setMaterialTaskSelection((current) => ({
																			...current,
																			[material.id]: event.target.checked
																				? [...selected, item.id]
																				: selected.filter(
																						(id) => id !== item.id,
																					),
																		}))
																	}
																/>
																{item.title}
															</label>
														))}
														<button
															type="button"
															className="text-xs text-primary underline-offset-4 hover:underline"
															disabled={
																materialUpdating || selected.length === 0
															}
															aria-label={`${t("home.privateDrafts.materialAttach")}: ${material.title}`}
															onClick={() => void associateMaterial(material)}
														>
															{t("home.privateDrafts.materialAttach")}
														</button>
														{selected.length === 0 && (
															<p className="text-xs text-muted-foreground">
																{t("home.privateDrafts.materialTaskRequired")}
															</p>
														)}
													</fieldset>
												)}
											</li>
										);
									})}
								</ul>
							) : (
								<p className="text-sm text-muted-foreground">
									{t("home.privateDrafts.materialEmpty")}
								</p>
							)}
						</div>
					)}
					{materialError && (
						<p className="mt-2 text-xs text-destructive" role="alert">
							{materialError}
						</p>
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
											{previewDraft?.state === "editable" && (
												<fieldset className="mt-2 space-y-1">
													<legend className="text-xs font-medium">
														{t("home.privateDrafts.materialTask")}
													</legend>
													{(previewDraft.items ?? []).map((item) => {
														const selected =
															materialTaskSelection[result.url] ?? [];
														return (
															<label
																key={item.id}
																className="flex items-center gap-2 text-xs"
															>
																<input
																	type="checkbox"
																	aria-label={`${t("home.privateDrafts.materialTask")}: ${result.title || result.url}: ${item.title}`}
																	checked={selected.includes(item.id)}
																	onChange={(event) =>
																		setMaterialTaskSelection((current) => ({
																			...current,
																			[result.url]: event.target.checked
																				? [...selected, item.id]
																				: selected.filter(
																						(id) => id !== item.id,
																					),
																		}))
																	}
																/>
																{item.title}
															</label>
														);
													})}
													<button
														type="button"
														className="text-xs text-primary underline-offset-4 hover:underline"
														disabled={
															materialUpdating ||
															(materialTaskSelection[result.url] ?? [])
																.length === 0
														}
														aria-label={`${t("home.privateDrafts.materialAttach")}: ${result.title || result.url}`}
														onClick={() =>
															void associateMaterial(
																{
																	id: webMaterialID(result.url),
																	kind: "document",
																	source_type: "web",
																	source_location: result.url,
																	source_version: result.accessed_at,
																	accessed_at: result.accessed_at,
																	read_status: result.read_status,
																	processed_status: result.processed_status,
																	purpose: result.snippet ?? "",
																},
																result.url,
															)
														}
													>
														{t("home.privateDrafts.materialAttach")}
													</button>
												</fieldset>
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
