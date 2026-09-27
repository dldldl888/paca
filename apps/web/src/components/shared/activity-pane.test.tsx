import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { forwardRef, useImperativeHandle } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { ActivityPane } from "./activity-pane";

const addComment = vi.fn();

vi.mock("@/components/shared/comment-blocknote", () => ({
	CommentDisplay: () => null,
	CommentEditor: forwardRef((_props: unknown, ref) => {
		useImperativeHandle(ref, () => ({
			getBlocks: () => [
				{
					type: "paragraph",
					content: [{ type: "text", text: "offline feedback" }],
				},
			],
			clear: vi.fn(),
			focus: vi.fn(),
		}));
		return <textarea aria-label="comment" />;
	}),
	blocksToText: () => "offline feedback",
}));
vi.mock("@/components/shared/content-diff", () => ({
	ContentDiffDialog: () => null,
}));

function Wrapper({ children }: { children: React.ReactNode }) {
	return (
		<QueryClientProvider
			client={
				new QueryClient({ defaultOptions: { queries: { retry: false } } })
			}
		>
			{children}
		</QueryClientProvider>
	);
}

describe("ActivityPane comment delivery", () => {
	beforeEach(() => {
		vi.clearAllMocks();
	});

	it("keeps a failed feedback draft available for retry", async () => {
		addComment
			.mockRejectedValueOnce(new Error("offline"))
			.mockResolvedValueOnce({ id: "comment-1" });
		const user = userEvent.setup();

		render(
			<Wrapper>
				<ActivityPane
					projectId="project-1"
					entityId="task-1"
					queryKey={["activities"]}
					queryFn={async () => []}
					addComment={addComment}
					describeActivity={() => null}
					getCommentBlocks={() => []}
				/>
			</Wrapper>,
		);

		await user.click(screen.getByRole("button"));
		await waitFor(() => expect(addComment).toHaveBeenCalledTimes(1));
		expect(await screen.findByRole("alert")).toHaveTextContent(
			/could not send/i,
		);

		await user.click(screen.getByRole("button", { name: /retry/i }));
		await waitFor(() => expect(addComment).toHaveBeenCalledTimes(2));
		expect(screen.queryByRole("alert")).not.toBeInTheDocument();
	});
});
