import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { getAgentDisplayName } from "@/components/projects/agents/agent-picker";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Textarea } from "@/components/ui/textarea";
import { useProjectPermissions } from "@/hooks/use-project-permissions";
import { agentsQueryOptions, conversationQueryOptions } from "@/lib/agent-api";
import {
	ApiErrorCode,
	getApiErrorCode,
	getApiErrorMessage,
} from "@/lib/api-error";
import {
	type Draft,
	type DraftContent,
	type DraftItem,
	type DraftPreview,
	draftQueryOptions,
	draftsQueryOptions,
	generateDraft,
	importDraft,
	previewDraft,
	publishDraft,
	saveDraft,
} from "@/lib/draft-api";
import { projectMembersQueryOptions } from "@/lib/project-api";

const importanceKeys = [
	"drafts.importance0",
	"drafts.importance1",
	"drafts.importance2",
	"drafts.importance3",
	"drafts.importance4",
] as const;
const emptyContent: DraftContent = {
	goal: "",
	items: [],
	sources: [],
	assumptions: [],
};
const selectClass = "h-9 w-full rounded-lg border bg-background px-2 text-sm";
const shanghai = {
	timeZone: "Asia/Shanghai",
	year: "numeric",
	month: "2-digit",
	day: "2-digit",
} as const;
function dateInput(value: string) {
	return value && !Number.isNaN(Date.parse(value))
		? new Intl.DateTimeFormat("sv-SE", shanghai).format(new Date(value))
		: "";
}
function displayDate(value: string) {
	return value && !Number.isNaN(Date.parse(value))
		? new Intl.DateTimeFormat("zh-CN", {
				...shanghai,
				hour: "2-digit",
				minute: "2-digit",
				hourCycle: "h23",
			}).format(new Date(value))
		: value || "—";
}
function publicationKey(draft: Draft) {
	return `paca:draft-publication:${draft.owner_user_id}:${draft.project_id}:${draft.id}:${draft.version}`;
}

export function DraftWorkspace({
	projectId,
	draftId,
	onSelectDraft,
}: {
	projectId: string;
	draftId?: string;
	onSelectDraft: (id?: string) => void;
}) {
	const { t } = useTranslation("projects");
	const qc = useQueryClient();
	const { hasProjectPermission } = useProjectPermissions(projectId);
	const allowed =
		hasProjectPermission("tasks.write") && hasProjectPermission("tasks.manage");
	const list = useQuery({ ...draftsQueryOptions(projectId), enabled: allowed });
	const saved = useQuery({
		...draftQueryOptions(projectId, draftId ?? ""),
		enabled: allowed && !!draftId,
		retry: false,
	});
	const members = useQuery({
		...projectMembersQueryOptions(projectId),
		enabled: allowed,
	});
	const projectAgents = useQuery({
		...agentsQueryOptions(projectId),
		enabled: allowed,
	});
	const humans = (members.data ?? []).filter(
		(m) => m.member_type !== "agent" && !!m.user_id,
	);
	const agents = (projectAgents.data ?? []).filter(
		(a) =>
			a.agent_type === "acp" &&
			a.acp_provider === "custom" &&
			a.handle === "hermes",
	);
	const [agentId, setAgentId] = useState("");
	const chosenAgent = agentId || agents[0]?.id || "";
	const [draft, setDraft] = useState<Draft>();
	const [content, setContent] = useState<DraftContent>(emptyContent);
	const [dirty, setDirty] = useState(false);
	const [preview, setPreview] = useState<DraftPreview>();
	const [error, setError] = useState("");
	const [conflict, setConflict] = useState(false);
	const [backup, setBackup] = useState("");
	const [busy, setBusy] = useState(false);
	const [publicationPending, setPublicationPending] = useState(false);
	const editingLocked = publicationPending || (!draft && busy);
	const inFlight = useRef(false);
	const revision = useRef(0);
	const mounted = useRef(true);
	useEffect(() => {
		mounted.current = true;
		return () => {
			mounted.current = false;
		};
	}, []);

	function accept(next: Draft) {
		setDraft(next);
		setContent({
			goal: next.goal,
			items: next.items ?? [],
			sources: next.sources ?? [],
			assumptions: next.assumptions ?? [],
		});
		setDirty(false);
		setPreview(undefined);
		qc.setQueryData(draftQueryOptions(projectId, next.id).queryKey, next);
		void qc.invalidateQueries({
			queryKey: draftsQueryOptions(projectId).queryKey,
			exact: true,
		});
	}
	// A background refetch must never replace a local edit. Reload is explicit.
	useEffect(() => {
		if (!draft && saved.data && !saved.isFetching) {
			setDraft(saved.data);
			setContent({
				goal: saved.data.goal,
				items: saved.data.items ?? [],
				sources: saved.data.sources ?? [],
				assumptions: saved.data.assumptions ?? [],
			});
		}
	}, [draft, saved.data, saved.isFetching]);
	useEffect(() => {
		if (!draft) return;
		try {
			setPublicationPending(
				draft.state !== "published" &&
					localStorage.getItem(`${publicationKey(draft)}:pending`) === "true",
			);
		} catch {
			setError(t("drafts.storageUnavailable"));
		}
	}, [draft, t]);

	const generation = draft?.generation;
	const generating =
		generation?.status === "starting" || generation?.status === "running";
	const conversation = useQuery({
		...conversationQueryOptions(projectId, generation?.conversation_id ?? ""),
		enabled: allowed && generating && !!generation?.conversation_id,
		refetchInterval: (query) =>
			["finished", "failed", "stopped"].includes(query.state.data?.status ?? "")
				? false
				: 2500,
		retry: false,
	});
	const terminal = ["finished", "failed", "stopped"].includes(
		conversation.data?.status ?? "",
	);
	const generationCurrent = generation?.version === draft?.version;
	const published = draft?.state === "published";

	function edit(next: DraftContent) {
		if (publicationPending || (!draft && inFlight.current)) return;
		revision.current += 1;
		setContent(next);
		setDirty(true);
		setPreview(undefined);
	}
	function editItem(id: string, patch: Partial<DraftItem>) {
		edit({
			...content,
			items: content.items.map((item) =>
				item.id === id ? { ...item, ...patch } : item,
			),
		});
	}
	function changed() {
		setConflict(true);
		setPreview(undefined);
		setError(t("drafts.conflict"));
	}
	async function run(action: () => Promise<void>) {
		if (inFlight.current) return;
		inFlight.current = true;
		setBusy(true);
		setError("");
		try {
			await action();
		} catch (err) {
			if (!mounted.current) return;
			if (getApiErrorCode(err) === ApiErrorCode.DraftConflict) changed();
			else if (getApiErrorCode(err) === ApiErrorCode.DraftGenerationPending)
				setError(t("drafts.generationPending"));
			else setError(getApiErrorMessage(err) || t("drafts.requestFailed"));
		} finally {
			inFlight.current = false;
			if (mounted.current) setBusy(false);
		}
	}
	async function update(action: () => Promise<Draft>) {
		const startedAt = revision.current;
		const next = await action();
		if (!mounted.current) return;
		if (revision.current !== startedAt) {
			changed();
			return;
		}
		accept(next);
		if (!draftId) onSelectDraft(next.id);
	}
	function reload() {
		void run(async () => {
			const startedAt = revision.current;
			const result = await saved.refetch();
			if (result.error) throw result.error;
			if (!result.data || !mounted.current) return;
			if (revision.current !== startedAt) {
				changed();
				return;
			}
			if (dirty) {
				setBackup(
					[
						content.goal,
						...content.items.map((item, index) =>
							[
								t("drafts.itemNumber", { number: index + 1 }),
								item.title,
								item.requirements,
								item.criteria,
								humans.find((m) => m.id === item.assignee_member_id)
									?.full_name ?? item.assignee_member_id,
								item.due_date,
								item.start_date,
								t("drafts.importance"),
								String(item.importance),
								...item.dependencies.map(
									(id) =>
										content.items.find((other) => other.id === id)?.title ?? id,
								),
							].join("\n"),
						),
						...content.sources,
						...content.assumptions,
					].join("\n\n"),
				);
			}
			accept(result.data);
			setConflict(false);
		});
	}
	function confirm(retryApproved: boolean) {
		if (
			!draft ||
			dirty ||
			conflict ||
			(retryApproved
				? !publicationPending
				: !preview ||
					preview.version !== draft.version ||
					!preview.can_publish ||
					preview.issues.length > 0)
		)
			return;
		void run(async () => {
			// Persist before the request; retain both ID and pending state across lost responses.
			const key = publicationKey(draft);
			let publicationId = localStorage.getItem(key);
			if (
				retryApproved &&
				(!publicationId || localStorage.getItem(`${key}:pending`) !== "true")
			)
				throw new Error(t("drafts.storageUnavailable"));
			if (!publicationId) {
				publicationId = crypto.randomUUID();
				localStorage.setItem(key, publicationId);
			}
			localStorage.setItem(`${key}:pending`, "true");
			setPublicationPending(true);
			try {
				const next = await publishDraft(
					projectId,
					{ id: draft.id, version: draft.version },
					publicationId,
				);
				localStorage.removeItem(`${key}:pending`);
				if (mounted.current) {
					setPublicationPending(false);
					accept(next);
				}
			} catch (err) {
				if (getApiErrorCode(err) === ApiErrorCode.BadRequest) {
					localStorage.removeItem(`${key}:pending`);
					setPublicationPending(false);
					setPreview(undefined);
				}
				throw err;
			}
		});
	}

	if (!allowed)
		return (
			<p className="p-6 text-sm text-muted-foreground">
				{t("drafts.forbidden")}
			</p>
		);
	if (saved.isError)
		return (
			<div role="alert" className="p-6">
				{t("drafts.unavailable")}{" "}
				<Button variant="outline" onClick={() => void saved.refetch()}>
					{t("drafts.retry")}
				</Button>
			</div>
		);
	const memberName = (id: string) =>
		humans.find((m) => m.id === id)?.full_name ||
		humans.find((m) => m.id === id)?.username ||
		t("drafts.unassigned");

	return (
		<div className="flex flex-1 flex-col overflow-auto p-4 md:p-6">
			<div className="mx-auto w-full max-w-5xl space-y-6">
				<header className="space-y-2">
					<h1 className="text-xl font-semibold">{t("drafts.title")}</h1>
					<p className="text-sm text-muted-foreground">{t("drafts.intro")}</p>
				</header>
				<div className="flex flex-wrap items-center gap-3">
					<label className="flex min-w-56 flex-1 items-center gap-2 text-sm">
						{t("drafts.mine")}
						<select
							aria-label={t("drafts.mine")}
							className={selectClass}
							value={draftId ?? ""}
							disabled={busy || dirty}
							onChange={(event) =>
								onSelectDraft(event.target.value || undefined)
							}
						>
							<option value="">{t("drafts.new")}</option>
							{(list.data ?? []).map((d) => (
								<option key={d.id} value={d.id}>
									{d.goal.slice(0, 60) || t("drafts.untitled")}
									{d.state === "published" ? ` · ${t("drafts.published")}` : ""}
								</option>
							))}
						</select>
					</label>
					<Button
						variant="outline"
						disabled={busy || dirty || !draftId}
						onClick={() => onSelectDraft()}
					>
						{t("drafts.new")}
					</Button>
				</div>
				{list.isError && <p role="alert">{t("drafts.listFailed")}</p>}
				{draftId && !draft ? (
					<p>{t("drafts.loading")}</p>
				) : (
					<>
						{error && (
							<div
								role="alert"
								className="rounded-lg border border-destructive/40 p-3 text-sm text-destructive"
							>
								{error}
								{conflict && (
									<Button
										variant="outline"
										className="ml-3"
										disabled={busy}
										onClick={reload}
									>
										{t("drafts.reload")}
									</Button>
								)}
							</div>
						)}
						{publicationPending && (
							<div className="rounded-lg border p-3 text-sm">
								{t("drafts.publicationPending")}
								<Button
									className="ml-3"
									variant="outline"
									disabled={busy}
									onClick={reload}
								>
									{t("drafts.checkPublication")}
								</Button>
								<Button
									className="ml-3"
									variant="outline"
									disabled={busy || conflict}
									onClick={() => confirm(true)}
								>
									{t("drafts.retryPublication")}
								</Button>
							</div>
						)}
						{backup && (
							<details className="rounded-lg border p-3 text-sm" open>
								<summary>{t("drafts.backup")}</summary>
								<Textarea
									aria-label={t("drafts.backup")}
									className="mt-3 min-h-40"
									readOnly
									value={backup}
								/>
							</details>
						)}
						{published ? (
							<section className="space-y-3 rounded-xl border p-5">
								<h2 className="font-semibold">{t("drafts.published")}</h2>
								<p className="text-sm text-muted-foreground">
									{t("drafts.publishedHelp")}
								</p>
								<ul className="space-y-2">
									{draft.published_tasks.map((task) => (
										<li key={task.task_id}>
											<a
												className="text-primary underline"
												href={`/projects/${projectId}/tasks/${task.task_id}`}
											>
												#{task.task_number} ·{" "}
												{
													draft.items.find((item) => item.id === task.item_id)
														?.title
												}
											</a>
										</li>
									))}
								</ul>
							</section>
						) : (
							<>
								<section className="space-y-4 rounded-xl border p-4 md:p-5">
									<label
										htmlFor="draft-goal"
										className="block space-y-2 text-sm font-medium"
									>
										{t("drafts.goal")}
										<Textarea
											disabled={editingLocked}
											id="draft-goal"
											aria-label={t("drafts.goal")}
											maxLength={10000}
											value={content.goal}
											onChange={(event) =>
												edit({ ...content, goal: event.target.value })
											}
										/>
									</label>
									<div className="flex flex-wrap items-center gap-3">
										<select
											className={`${selectClass} max-w-64`}
											aria-label={t("drafts.assistant")}
											value={chosenAgent}
											onChange={(event) => setAgentId(event.target.value)}
										>
											{agents.length === 0 && (
												<option value="">{t("drafts.noAssistant")}</option>
											)}
											{agents.map((agent) => (
												<option key={agent.id} value={agent.id}>
													{getAgentDisplayName(agent)}
												</option>
											))}
										</select>
										<Button
											variant="outline"
											disabled={
												busy ||
												dirty ||
												conflict ||
												publicationPending ||
												!draft ||
												!chosenAgent ||
												!content.goal.trim()
											}
											onClick={() =>
												draft &&
												void run(() =>
													update(() =>
														generateDraft(projectId, draft, chosenAgent),
													),
												)
											}
										>
											{generation
												? t("drafts.regenerate")
												: t("drafts.generate")}
										</Button>
									</div>
									{generation && (
										<div className="space-y-2 rounded-lg bg-muted/40 p-3 text-sm">
											<p>
												{t("drafts.generation")}:{" "}
												{generation.status === "failed"
													? t("drafts.generationFailed")
													: generation.status === "imported"
														? t("drafts.imported")
														: terminal
															? t("drafts.generationReady")
															: t("drafts.generationPending")}
											</p>
											{generation.error && (
												<p role="alert" className="text-destructive">
													{generation.error}
												</p>
											)}
											{conversation.isError && (
												<p role="alert">{t("drafts.conversationFailed")}</p>
											)}
											{generation.conversation_id && (
												<a
													className="text-primary underline"
													href={`/projects/${projectId}/conversations/${generation.conversation_id}`}
													target="_blank"
													rel="noreferrer"
												>
													{t("drafts.rawConversation")}
												</a>
											)}
											{generating && terminal && (
												<Button
													className="ml-3"
													variant="outline"
													disabled={
														busy ||
														dirty ||
														conflict ||
														publicationPending ||
														!generationCurrent
													}
													onClick={() =>
														draft &&
														void run(() =>
															update(() =>
																importDraft(projectId, draft, generation.id),
															),
														)
													}
												>
													{t("drafts.import")}
												</Button>
											)}
											{generating && (dirty || !generationCurrent) && (
												<p>{t("drafts.generationEdited")}</p>
											)}
										</div>
									)}
									{(content.sources.length > 0 ||
										content.assumptions.length > 0) && (
										<details className="text-sm">
											<summary className="cursor-pointer">
												{t("drafts.privateContext")}
											</summary>
											{content.sources.length > 0 && (
												<div className="mt-3">
													<h3 className="font-medium">{t("drafts.sources")}</h3>
													<p className="whitespace-pre-wrap break-words">
														{content.sources.join("\n")}
													</p>
												</div>
											)}
											{content.assumptions.length > 0 && (
												<div className="mt-3">
													<h3 className="font-medium">
														{t("drafts.assumptions")}
													</h3>
													<p className="whitespace-pre-wrap break-words">
														{content.assumptions.join("\n")}
													</p>
												</div>
											)}
										</details>
									)}
								</section>
								<section className="space-y-4">
									<div>
										<h2 className="font-semibold">{t("drafts.items")}</h2>
										<p className="mt-1 text-sm text-muted-foreground">
											{t("drafts.timezone")}
										</p>
									</div>
									{content.items.map((item, index) => (
										<fieldset
											key={item.id}
											className="space-y-4 rounded-xl border p-4 md:p-5"
											disabled={editingLocked}
										>
											<legend className="px-2 text-sm font-medium">
												{t("drafts.itemNumber", { number: index + 1 })}
											</legend>
											{(["title", "requirements", "criteria"] as const).map(
												(field) => (
													<label
														key={field}
														htmlFor={`${item.id}-${field}`}
														className="block space-y-1 text-sm"
													>
														{t(
															field === "title"
																? "drafts.itemTitle"
																: `drafts.${field}`,
														)}
														{field === "title" ? (
															<Input
																aria-label={`${t("drafts.itemTitle")} ${index + 1}`}
																id={`${item.id}-${field}`}
																value={item[field]}
																maxLength={200}
																onChange={(event) =>
																	editItem(item.id, {
																		[field]: event.target.value,
																	})
																}
															/>
														) : (
															<Textarea
																aria-label={`${t(`drafts.${field}`)} ${index + 1}`}
																id={`${item.id}-${field}`}
																value={item[field]}
																maxLength={10000}
																onChange={(event) =>
																	editItem(item.id, {
																		[field]: event.target.value,
																	})
																}
															/>
														)}
													</label>
												),
											)}
											<div className="grid gap-4 sm:grid-cols-2">
												<label className="space-y-1 text-sm">
													{t("drafts.assignee")}
													<select
														className={selectClass}
														aria-label={`${t("drafts.assignee")} ${index + 1}`}
														value={item.assignee_member_id}
														onChange={(event) =>
															editItem(item.id, {
																assignee_member_id: event.target.value,
															})
														}
													>
														<option value="">{t("drafts.unassigned")}</option>
														{item.assignee_member_id &&
															!humans.some(
																(m) => m.id === item.assignee_member_id,
															) && (
																<option value={item.assignee_member_id}>
																	{t("drafts.invalidAssignee")}
																</option>
															)}
														{humans.map((member) => (
															<option key={member.id} value={member.id}>
																{member.full_name || member.username}
															</option>
														))}
													</select>
												</label>
												<label className="space-y-1 text-sm">
													{t("drafts.importance")}
													<select
														className={selectClass}
														aria-label={`${t("drafts.importance")} ${index + 1}`}
														value={item.importance}
														onChange={(event) =>
															editItem(item.id, {
																importance: Number(event.target.value),
															})
														}
													>
														{[0, 1, 2, 3, 4].map((importance) => (
															<option key={importance} value={importance}>
																{t(importanceKeys[importance])}
															</option>
														))}
													</select>
												</label>
												{(["due_date", "start_date"] as const).map((field) => (
													<label
														key={field}
														htmlFor={`${item.id}-${field}`}
														className="space-y-1 text-sm"
													>
														{t(`drafts.${field}`)}
														<Input
															id={`${item.id}-${field}`}
															type="date"
															aria-label={`${t(`drafts.${field}`)} ${index + 1}`}
															value={dateInput(item[field])}
															onChange={(event) =>
																editItem(item.id, {
																	[field]: event.target.value
																		? `${event.target.value}T${field === "due_date" ? "23:59" : "00:00"}:00+08:00`
																		: "",
																})
															}
														/>
														<span className="block text-xs text-muted-foreground">
															{displayDate(item[field])}
														</span>
													</label>
												))}
											</div>
											<fieldset className="space-y-2">
												<legend className="mb-2 text-sm">
													{t("drafts.dependencies")}
												</legend>
												{content.items
													.filter((other) => other.id !== item.id)
													.map((other) => (
														<label
															key={other.id}
															className="mr-4 inline-flex items-center gap-2 text-sm"
														>
															<input
																type="checkbox"
																checked={item.dependencies.includes(other.id)}
																onChange={(event) =>
																	editItem(item.id, {
																		dependencies: event.target.checked
																			? [...item.dependencies, other.id]
																			: item.dependencies.filter(
																					(id) => id !== other.id,
																				),
																	})
																}
															/>
															{other.title || t("drafts.untitled")}
														</label>
													))}
											</fieldset>
											<Button
												variant="destructive"
												onClick={() =>
													edit({
														...content,
														items: content.items
															.filter((other) => other.id !== item.id)
															.map((other) => ({
																...other,
																dependencies: other.dependencies.filter(
																	(id) => id !== item.id,
																),
															})),
													})
												}
											>
												{t("drafts.remove", { number: index + 1 })}
											</Button>
										</fieldset>
									))}
									<Button
										variant="outline"
										disabled={editingLocked || content.items.length >= 30}
										onClick={() =>
											edit({
												...content,
												items: [
													...content.items,
													{
														id: crypto.randomUUID(),
														title: "",
														requirements: "",
														criteria: "",
														assignee_member_id: "",
														due_date: "",
														start_date: "",
														importance: 0,
														dependencies: [],
													},
												],
											})
										}
									>
										{t("drafts.add")}
									</Button>
								</section>
								<div className="flex flex-wrap items-center gap-3">
									<Button
										disabled={
											busy ||
											conflict ||
											publicationPending ||
											(!dirty && !!draft)
										}
										onClick={() =>
											void run(() =>
												update(() => saveDraft(projectId, content, draft)),
											)
										}
									>
										{t("drafts.save")}
									</Button>
									<Button
										variant="outline"
										disabled={busy || dirty || conflict || !draft}
										onClick={() =>
											draft &&
											void run(async () => {
												const startedAt = revision.current;
												const next = await previewDraft(projectId, draft);
												if (mounted.current && revision.current === startedAt)
													setPreview(next);
											})
										}
									>
										{t("drafts.preview")}
									</Button>
									{(dirty || !draft) && (
										<span className="text-sm text-muted-foreground">
											{t("drafts.saveFirst")}
										</span>
									)}
								</div>
								{preview && !dirty && (
									<section
										aria-label={t("drafts.employeeContent")}
										className="space-y-4 rounded-xl border border-primary/40 p-4 md:p-5"
									>
										<h2 className="font-semibold">
											{t("drafts.employeeContent")}
										</h2>
										<p className="text-sm text-muted-foreground">
											{t("drafts.previewHelp")}
										</p>
										{preview.issues.length > 0 && (
											<ul
												role="alert"
												className="list-inside list-disc text-sm text-destructive"
											>
												{preview.issues.map((issue) => (
													<li key={`${issue.field}-${issue.message}`}>
														{issue.message}
													</li>
												))}
											</ul>
										)}
										{preview.items.map((item, index) => (
											<article
												key={item.id}
												className="space-y-2 border-t pt-4 text-sm"
											>
												<h3 className="font-semibold">
													{index + 1}. <span>{item.title}</span>
												</h3>
												<dl className="space-y-2">
													<div>
														<dt className="font-medium">
															{t("drafts.requirements")}
														</dt>
														<dd className="whitespace-pre-wrap break-words">
															{item.requirements}
														</dd>
													</div>
													<div>
														<dt className="font-medium">
															{t("drafts.criteria")}
														</dt>
														<dd className="whitespace-pre-wrap break-words">
															{item.criteria}
														</dd>
													</div>
													<div>
														<dt className="inline font-medium">
															{t("drafts.assignee")}:{" "}
														</dt>
														<dd className="inline">
															{memberName(item.assignee_member_id)}
														</dd>
													</div>
													<div>
														<dt className="inline font-medium">
															{t("drafts.due_date")}:{" "}
														</dt>
														<dd className="inline">
															{displayDate(item.due_date)} (Asia/Shanghai)
														</dd>
													</div>
													{item.start_date && (
														<div>
															<dt className="inline font-medium">
																{t("drafts.start_date")}:{" "}
															</dt>
															<dd className="inline">
																{displayDate(item.start_date)} (Asia/Shanghai)
															</dd>
														</div>
													)}
													<div>
														<dt className="inline font-medium">
															{t("drafts.importance")}:{" "}
														</dt>
														<dd className="inline">
															{t(
																importanceKeys[item.importance] ??
																	importanceKeys[0],
															)}
														</dd>
													</div>
													{item.dependencies.length > 0 && (
														<div>
															<dt className="font-medium">
																{t("drafts.dependencies")}
															</dt>
															<dd>
																{item.dependencies
																	.map(
																		(id) =>
																			preview.items.find(
																				(other) => other.id === id,
																			)?.title || t("drafts.untitled"),
																	)
																	.join("、")}
															</dd>
														</div>
													)}
												</dl>
											</article>
										))}
										{preview.can_publish &&
											preview.issues.length === 0 &&
											preview.version === draft?.version && (
												<div className="space-y-3 border-t pt-4">
													<p className="text-sm">{t("drafts.confirmHelp")}</p>
													<Button
														disabled={busy || conflict}
														onClick={() => confirm(false)}
													>
														{t("drafts.confirm")}
													</Button>
												</div>
											)}
									</section>
								)}
							</>
						)}
					</>
				)}
			</div>
		</div>
	);
}
