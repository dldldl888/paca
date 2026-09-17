import { queryOptions } from "@tanstack/react-query";
import { apiClient } from "./api-client";
import type { SuccessEnvelope } from "./api-error";

export interface DraftItem {
	id: string;
	title: string;
	requirements: string;
	criteria: string;
	assignee_member_id: string;
	due_date: string;
	start_date: string;
	importance: number;
	dependencies: string[];
}
export interface DraftContent {
	goal: string;
	items: DraftItem[];
	sources: string[];
	assumptions: string[];
}
export interface Draft extends DraftContent {
	id: string;
	project_id: string;
	owner_user_id: string;
	version: number;
	state: "editable" | "published";
	generation?: {
		id: string;
		version: number;
		agent_id: string;
		status: "starting" | "running" | "imported" | "failed";
		conversation_id?: string;
		chat_session_id?: string;
		error?: string;
	};
	publication_id?: string;
	published_version?: number;
	published_tasks: { item_id: string; task_id: string; task_number: number }[];
	published_at?: string;
	created_at: string;
	updated_at: string;
}
export interface DraftPreview {
	id: string;
	project_id: string;
	version: number;
	items: DraftItem[];
	issues: { field: string; message: string }[];
	can_publish: boolean;
}
const base = (projectId: string) => `/projects/${projectId}/drafts`;
export const draftsQueryOptions = (projectId: string) =>
	queryOptions({
		queryKey: ["projects", projectId, "drafts"],
		queryFn: async () =>
			(
				await apiClient.instance.get<SuccessEnvelope<{ items: Draft[] }>>(
					base(projectId),
				)
			).data.data.items,
	});
export const draftQueryOptions = (projectId: string, draftId: string) =>
	queryOptions({
		queryKey: ["projects", projectId, "drafts", draftId],
		queryFn: async () =>
			(
				await apiClient.instance.get<SuccessEnvelope<Draft>>(
					`${base(projectId)}/${draftId}`,
				)
			).data.data,
	});
export async function saveDraft(
	projectId: string,
	content: DraftContent,
	draft?: Draft,
): Promise<Draft> {
	// Whitelist content even when the caller holds a full Draft object.
	const { goal, items, sources, assumptions } = content;
	const body = { goal, items, sources, assumptions };
	const response = draft
		? await apiClient.instance.patch<SuccessEnvelope<Draft>>(
				`${base(projectId)}/${draft.id}`,
				{ ...body, expected_version: draft.version },
			)
		: await apiClient.instance.post<SuccessEnvelope<Draft>>(
				base(projectId),
				body,
			);
	return response.data.data;
}
export async function previewDraft(
	projectId: string,
	draft: Draft,
): Promise<DraftPreview> {
	return (
		await apiClient.instance.post<SuccessEnvelope<DraftPreview>>(
			`${base(projectId)}/${draft.id}/preview`,
			{ expected_version: draft.version },
		)
	).data.data;
}
export async function generateDraft(
	projectId: string,
	draft: Draft,
	agentId: string,
): Promise<Draft> {
	return (
		await apiClient.instance.post<SuccessEnvelope<Draft>>(
			`${base(projectId)}/${draft.id}/generate`,
			{ expected_version: draft.version, agent_id: agentId },
		)
	).data.data;
}
export async function importDraft(
	projectId: string,
	draft: Draft,
	generationId: string,
): Promise<Draft> {
	return (
		await apiClient.instance.post<SuccessEnvelope<Draft>>(
			`${base(projectId)}/${draft.id}/import`,
			{ expected_version: draft.version, generation_id: generationId },
		)
	).data.data;
}
export async function publishDraft(
	projectId: string,
	preview: Pick<DraftPreview, "id" | "version">,
	publicationId: string,
): Promise<Draft> {
	return (
		await apiClient.instance.post<SuccessEnvelope<Draft>>(
			`${base(projectId)}/${preview.id}/publish`,
			{
				expected_version: preview.version,
				publication_id: publicationId,
				confirmed: true,
			},
		)
	).data.data;
}
