import { beforeEach, describe, expect, it, vi } from "vitest";

const { mockPost } = vi.hoisted(() => ({ mockPost: vi.fn() }));

vi.mock("./api-client", () => ({
	apiClient: { instance: { post: mockPost } },
}));

import {
	ProjectDraftUploadError,
	uploadProjectDraftFile,
} from "./project-draft-api";

describe("uploadProjectDraftFile", () => {
	beforeEach(() => {
		vi.clearAllMocks();
	});

	it("keeps a persisted file id when retry initiation fails", async () => {
		mockPost.mockRejectedValueOnce({
			response: {
				status: 503,
				data: { error: "file service temporarily unavailable" },
			},
		});

		const error = await uploadProjectDraftFile(
			"draft-1",
			new File(["content"], "brief.pdf", { type: "application/pdf" }),
			"file-failed",
		).catch((value) => value);

		expect(error).toBeInstanceOf(ProjectDraftUploadError);
		expect(error).toMatchObject({
			fileId: "file-failed",
			message: "file service temporarily unavailable",
		});
		expect(mockPost).toHaveBeenCalledWith(
			"/project-drafts/draft-1/files/retry-upload",
			{ file_id: "file-failed" },
		);
	});
});
