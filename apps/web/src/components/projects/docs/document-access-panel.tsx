import {
	useInfiniteQuery,
	useMutation,
	useQueries,
	useQueryClient,
} from "@tanstack/react-query";
import { useId, useState } from "react";
import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { useTaskPickerSearch } from "@/hooks/use-task-picker-search";
import { ApiErrorCode, getApiErrorCode } from "@/lib/api-error";
import {
	type Document,
	docQueryKeys,
	docQueryOptions,
	type UpdateDocumentAccessInput,
	updateDocumentAccess,
} from "@/lib/doc-api";
import {
	taskQueryOptions,
	tasksPickerInfiniteQueryOptions,
} from "@/lib/interaction-api";

export function DocumentAccessPanel({
	projectId,
	document,
	canManage,
}: {
	projectId: string;
	document: Document;
	canManage: boolean;
}) {
	const { t } = useTranslation("projects");
	const qc = useQueryClient();
	const scopeId = useId();
	const [draft, setDraft] = useState<UpdateDocumentAccessInput | null>(null);
	const [conflict, setConflict] = useState(false);
	const [refreshing, setRefreshing] = useState(false);
	const [message, setMessage] = useState<string | null>(null);
	const editable = canManage && document.visibility !== "legacy_project";
	const picking = editable && draft?.visibility === "task";
	const list = useInfiniteQuery({
		...tasksPickerInfiniteQueryOptions(projectId),
		enabled: picking,
	});
	const search = useTaskPickerSearch(projectId, picking);
	const selectedIds = draft?.task_ids ?? document.task_ids ?? [];
	const selected = useQueries({
		queries: selectedIds.map((id) => taskQueryOptions(projectId, id)),
	});
	const tasks = search.isSearching
		? search.results
		: (list.data?.pages.flatMap((page) => page.items) ?? []);
	const pagination = search.isSearching
		? search.pagination
		: {
				hasMore: !!list.hasNextPage,
				isLoadingMore: list.isFetchingNextPage,
				onLoadMore: () => void list.fetchNextPage(),
			};
	const errorText = (error: unknown) => {
		const code = getApiErrorCode(error);
		if (code === ApiErrorCode.DocumentAccessConflict)
			return t("docs.access.conflict");
		if (
			code === ApiErrorCode.Forbidden ||
			(error as { response?: { status?: number } })?.response?.status === 404
		)
			return t("docs.access.forbidden");
		return t("docs.access.failed");
	};
	const save = useMutation({
		mutationFn: (input: UpdateDocumentAccessInput) =>
			updateDocumentAccess(projectId, document.id, input),
		onSuccess: () => {
			setDraft(null);
			setMessage(t("docs.access.saved"));
			// The PATCH result is partial. Read the authoritative full document again
			// and evict sensitive child data whose authorization may have changed.
			void qc.invalidateQueries({
				queryKey: docQueryKeys.all(projectId),
			});
		},
		onError: (error) => {
			setConflict(
				getApiErrorCode(error) === ApiErrorCode.DocumentAccessConflict,
			);
			setMessage(errorText(error));
		},
	});
	const busy = save.isPending || refreshing;
	const refresh = async () => {
		setRefreshing(true);
		try {
			const latest = await qc.fetchQuery({
				...docQueryOptions(projectId, document.id),
				staleTime: 0,
			});
			// Only this explicit refresh rebases the user's pending selection.
			setDraft(
				(input) =>
					input && { ...input, expected_access_version: latest.access_version },
			);
			setConflict(false);
			setMessage(t("docs.access.recheck"));
		} catch (error) {
			setMessage(errorText(error));
		} finally {
			setRefreshing(false);
		}
	};
	const toggleTask = (id: string) =>
		setDraft(
			(input) =>
				input && {
					...input,
					task_ids: input.task_ids.includes(id)
						? input.task_ids.filter((item) => item !== id)
						: [...input.task_ids, id],
				},
		);
	const scope = document.visibility;
	const hasSourceLink =
		document.source_document_id && document.source_project_id;

	return (
		<section
			aria-label={t("docs.access.title")}
			className="mb-6 space-y-3 rounded-lg border border-border/50 p-4 text-sm"
		>
			<div className="flex flex-wrap items-center justify-between gap-2">
				<p>
					{t("docs.access.current", {
						scope: t(`docs.access.scopes.${scope}`),
					})}
				</p>
				{editable && !draft && (
					<Button
						variant="outline"
						size="sm"
						onClick={() => {
							setDraft({
								visibility: scope as UpdateDocumentAccessInput["visibility"],
								task_ids: document.task_ids ?? [],
								expected_access_version: document.access_version,
							});
							setConflict(false);
							setMessage(null);
						}}
					>
						{t("docs.access.manage")}
					</Button>
				)}
			</div>
			{document.source_document_id && (
				<div className="space-y-1 text-muted-foreground">
					<p>
						{t("docs.access.source")}:{" "}
						{hasSourceLink ? (
							<a
								className="underline"
								href={`/projects/${document.source_project_id}/docs/${document.source_document_id}`}
							>
								{document.source_title || t("docs.access.openSource")}
							</a>
						) : (
							document.source_title || t("docs.access.sourceUnavailable")
						)}
					</p>
					{document.source_updated_at && (
						<p>
							{t("docs.access.sourceUpdated", {
								date: new Date(document.source_updated_at).toLocaleString(),
							})}
						</p>
					)}
					<p>{t("docs.access.sourceHint")}</p>
				</div>
			)}
			{selectedIds.length > 0 && (
				<ul className="space-y-1">
					{selectedIds.map((id, index) => (
						<li key={id} className="flex items-center justify-between gap-2">
							<span className="min-w-0 break-words">
								{selected[index]?.data
									? `#${selected[index].data.task_number} ${selected[index].data.title}`
									: t("docs.access.taskUnavailable")}
							</span>
							{draft && editable && (
								<Button
									size="sm"
									variant="ghost"
									disabled={busy}
									onClick={() => toggleTask(id)}
									aria-label={t("docs.access.removeTask", {
										title:
											selected[index]?.data?.title ??
											t("docs.access.taskUnavailable"),
									})}
								>
									{t("docs.access.remove")}
								</Button>
							)}
						</li>
					))}
				</ul>
			)}
			{draft && editable && (
				<div className="space-y-3">
					<label className="block space-y-1" htmlFor={scopeId}>
						<span>{t("docs.access.visibility")}</span>
						<select
							id={scopeId}
							className="block h-9 w-full rounded-md border bg-background px-2"
							value={draft.visibility}
							disabled={busy}
							onChange={(event) =>
								setDraft({
									...draft,
									visibility: event.target
										.value as UpdateDocumentAccessInput["visibility"],
									task_ids: event.target.value === "task" ? draft.task_ids : [],
								})
							}
						>
							{(["private", "project", "task"] as const).map((value) => (
								<option
									key={value}
									value={value}
									disabled={value === "private" && !document.owner_user_id}
								>
									{t(`docs.access.scopes.${value}`)}
								</option>
							))}
						</select>
					</label>
					<p className="text-muted-foreground">
						{t(`docs.access.hints.${draft.visibility}`)}
					</p>
					{!document.owner_user_id && <p>{t("docs.access.noOwner")}</p>}
					{picking && (
						<fieldset disabled={busy} className="space-y-2">
							<legend className="mb-2">{t("docs.access.tasks")}</legend>
							<Input
								aria-label={t("docs.access.search")}
								placeholder={t("docs.access.search")}
								value={search.search}
								onChange={(event) => search.setSearch(event.target.value)}
							/>
							<div className="max-h-48 space-y-2 overflow-y-auto">
								{(search.isSearching ? search.isLoading : list.isPending) ? (
									<p>{t("docs.access.loading")}</p>
								) : (
									tasks.map((task) => (
										<label key={task.id} className="flex items-start gap-2">
											<input
												type="checkbox"
												className="mt-1"
												checked={draft.task_ids.includes(task.id)}
												onChange={() => toggleTask(task.id)}
											/>
											<span className="break-words">
												#{task.task_number} {task.title}
											</span>
										</label>
									))
								)}
							</div>
							{!search.isLoading && !list.isPending && tasks.length === 0 && (
								<p>
									{t(
										search.isError || list.isError
											? "docs.access.failed"
											: "docs.access.noTasks",
									)}
								</p>
							)}
							{pagination.hasMore && (
								<Button
									variant="outline"
									size="sm"
									disabled={pagination.isLoadingMore}
									onClick={pagination.onLoadMore}
								>
									{t("docs.access.loadMore")}
								</Button>
							)}
							{draft.task_ids.length === 0 && (
								<p>{t("docs.access.selectTask")}</p>
							)}
						</fieldset>
					)}
					<div className="flex flex-wrap gap-2">
						<Button
							disabled={
								busy ||
								conflict ||
								(draft.visibility === "task" && draft.task_ids.length === 0)
							}
							onClick={() => save.mutate(draft)}
						>
							{t(save.isPending ? "docs.access.saving" : "docs.access.save")}
						</Button>
						<Button
							variant="outline"
							disabled={busy}
							onClick={() => {
								setDraft(null);
								setConflict(false);
								setMessage(null);
							}}
						>
							{t("docs.access.cancel")}
						</Button>
						{conflict && (
							<Button
								variant="outline"
								disabled={busy}
								onClick={() => void refresh()}
							>
								{t("docs.access.refresh")}
							</Button>
						)}
					</div>
				</div>
			)}
			{message && <p role="status">{message}</p>}
		</section>
	);
}
