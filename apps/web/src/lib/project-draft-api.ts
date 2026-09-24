import { queryOptions } from "@tanstack/react-query";

import { apiClient } from "./api-client";
import type { SuccessEnvelope } from "./api-error";

export interface ProjectDraftGeneration {
	id: string;
	version: number;
	agent_id: string;
	status: "starting" | "running" | "imported" | "failed";
	base_event_index: number;
	conversation_id?: string;
	error?: string;
}

export interface ProjectDraft {
	id: string;
	name: string;
	goal: string;
	manager_user_id?: string;
	items: Array<{
		id: string;
		title: string;
		requirements: string;
		criteria: string;
		assignee_user_id?: string;
		due_date?: string;
		start_date?: string;
		importance: number;
		dependencies: string[];
	}>;
	participants: Array<{ user_id: string; role?: string }>;
	materials: Array<{
		id: string;
		kind: "document" | "file";
		source_type?: "internal" | "upload" | "web" | "ai_summary";
		source_location?: string;
		source_version?: string;
		processed_status?: "pending" | "ready" | "failed" | "unverified";
		purpose?: string;
		task_ids?: string[];
		required: boolean;
	}>;
	chat_session_id: string;
	version: number;
	state: "editable" | "published";
	publication_id?: string;
	published_project_id?: string;
	generation?: ProjectDraftGeneration;
	created_at: string;
	updated_at: string;
}

export interface ProjectDraftMaterialCandidate {
	id: string;
	project_id: string;
	title: string;
	source_type: "internal";
	source_location: string;
	source_version: string;
	processed_status: "ready";
	visibility: string;
}

export interface ProjectDraftFile {
	id: string;
	draft_id: string;
	file_id: string;
	file_name: string;
	content_type: string;
	file_size: number;
	upload_status: "pending" | "uploaded" | "failed";
	processing_status: "pending" | "ready" | "failed" | "unverified";
	processing_error?: string;
	created_at: string;
	updated_at: string;
}

export class ProjectDraftUploadError extends Error {
	readonly fileId: string;

	constructor(fileId: string, message = "project draft file upload failed") {
		super(message);
		this.name = "ProjectDraftUploadError";
		this.fileId = fileId;
	}
}

export interface ProjectDraftWebCandidate {
	title: string;
	url: string;
	snippet?: string;
	source_type: "web";
	accessed_at: string;
	read_status: "unverified";
	processed_status: "unverified";
}

export interface ProjectDraftPreview {
	id: string;
	version: number;
	name: string;
	goal: string;
	manager_user_id?: string;
	items: ProjectDraft["items"];
	participants: ProjectDraft["participants"];
	materials: ProjectDraft["materials"];
	employee_views: Array<{
		user_id: string;
		items: ProjectDraft["items"];
		materials: ProjectDraft["materials"];
	}>;
	issues: Array<{ field: string; message: string }>;
	can_publish: boolean;
}

export interface ProjectDraftPublication {
	publication_id: string;
	project_id: string;
	task_ids: Record<string, string>;
}

export const projectDraftsQueryOptions = queryOptions({
	queryKey: ["private-project-drafts"],
	queryFn: async () =>
		(
			await apiClient.instance.get<SuccessEnvelope<{ items: ProjectDraft[] }>>(
				"/project-drafts",
			)
		).data.data.items,
});

export const projectDraftMaterialsQueryOptions = (search: string) =>
	queryOptions({
		queryKey: ["private-project-draft-materials", search],
		queryFn: async () =>
			(
				await apiClient.instance.get<
					SuccessEnvelope<{
						items: ProjectDraftMaterialCandidate[];
					}>
				>("/project-drafts/materials", {
					params: { search, limit: 20 },
				})
			).data.data.items,
	});

export const projectDraftFilesQueryOptions = (draftId: string) =>
	queryOptions({
		queryKey: ["private-project-draft-files", draftId],
		queryFn: async () =>
			(
				await apiClient.instance.get<
					SuccessEnvelope<{ items: ProjectDraftFile[] }>
				>(`/project-drafts/${draftId}/files`)
			).data.data.items,
	});

export const projectDraftWebSearchQueryOptions = (query: string) =>
	queryOptions({
		queryKey: ["private-project-draft-web-search", query],
		queryFn: async () =>
			(
				await apiClient.instance.get<
					SuccessEnvelope<{ items: ProjectDraftWebCandidate[] }>
				>("/project-drafts/web-search", { params: { q: query, limit: 20 } })
			).data.data.items,
	});

export const projectDraftPreviewQueryOptions = (
	draftId: string,
	version: number,
) =>
	queryOptions({
		queryKey: ["private-project-draft-preview", draftId, version],
		queryFn: async () =>
			(
				await apiClient.instance.post<SuccessEnvelope<ProjectDraftPreview>>(
					`/project-drafts/${draftId}/preview`,
					{ expected_version: version },
				)
			).data.data,
	});

export async function publishProjectDraft(
	draftId: string,
	expectedVersion: number,
	publicationId: string,
): Promise<ProjectDraftPublication> {
	return (
		await apiClient.instance.post<SuccessEnvelope<ProjectDraftPublication>>(
			`/project-drafts/${draftId}/publish`,
			{
				expected_version: expectedVersion,
				publication_id: publicationId,
			},
		)
	).data.data;
}

export async function uploadProjectDraftFile(
	draftId: string,
	file: File,
	retryFileId?: string,
) {
	const session = retryFileId
		? (
				await apiClient.instance.post<
					SuccessEnvelope<{ file_id: string; upload_url: string }>
				>(`/project-drafts/${draftId}/files/retry-upload`, {
					file_id: retryFileId,
				})
			).data.data
		: (
				await apiClient.instance.post<
					SuccessEnvelope<{ file_id: string; upload_url: string }>
				>(`/project-drafts/${draftId}/files/initiate-upload`, {
					file_name: file.name,
					content_type: file.type || "application/octet-stream",
					file_size: file.size,
				})
			).data.data;
	try {
		const uploaded = await fetch(session.upload_url, {
			method: "PUT",
			headers: { "Content-Type": file.type || "application/octet-stream" },
			body: file,
		});
		if (!uploaded.ok) throw new Error("upload failed");
	} catch (error) {
		if (error instanceof ProjectDraftUploadError) throw error;
		throw new ProjectDraftUploadError(session.file_id, "upload failed");
	}
	try {
		const completed = (
			await apiClient.instance.post<SuccessEnvelope<ProjectDraftFile>>(
				`/project-drafts/${draftId}/files/complete-upload`,
				{ file_id: session.file_id },
			)
		).data.data;
		if (completed.processing_status === "failed") {
			throw new ProjectDraftUploadError(
				session.file_id,
				completed.processing_error || "file processing failed",
			);
		}
		return completed;
	} catch (error) {
		if (error instanceof ProjectDraftUploadError) throw error;
		throw new ProjectDraftUploadError(
			session.file_id,
			"file processing failed",
		);
	}
}
