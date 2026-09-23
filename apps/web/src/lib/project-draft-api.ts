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
	state: "editable";
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
