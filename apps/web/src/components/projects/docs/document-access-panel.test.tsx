import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const { mockGetDocument, mockSearch, mockUpdateDocumentAccess } = vi.hoisted(
	() => ({
		mockGetDocument: vi.fn(),
		mockSearch: {
			search: "",
			setSearch: vi.fn(),
			isSearching: false,
			results: [],
			isLoading: false,
			isError: false,
			pagination: {
				hasMore: false,
				isLoadingMore: false,
				onLoadMore: vi.fn(),
			},
		},
		mockUpdateDocumentAccess: vi.fn(),
	}),
);

vi.mock("@/lib/interaction-api", () => ({
	taskQueryOptions: (_projectId: string, taskId: string) => ({
		queryKey: ["task", taskId],
		queryFn: async () => (taskId === "task-a" ? taskA : taskB),
	}),
	tasksPickerInfiniteQueryOptions: () => ({
		queryKey: ["tasks-picker"],
		queryFn: async () => ({ items: [taskA, taskB], next_cursor: null }),
		initialPageParam: undefined,
		getNextPageParam: () => undefined,
	}),
}));

vi.mock("@/hooks/use-task-picker-search", () => ({
	useTaskPickerSearch: () => mockSearch,
}));

vi.mock("@/lib/doc-api", async () => ({
	...(await vi.importActual<typeof import("@/lib/doc-api")>("@/lib/doc-api")),
	docQueryOptions: (projectId: string, docId: string) => ({
		queryKey: ["projects", projectId, "docs", docId],
		queryFn: mockGetDocument,
	}),
	updateDocumentAccess: mockUpdateDocumentAccess,
}));

import { type Document, docQueryKeys } from "@/lib/doc-api";
import { DocumentAccessPanel } from "./document-access-panel";

const taskA = {
	id: "task-a",
	project_id: "project-1",
	title: "Prepare the report",
	task_number: 12,
};
const taskB = {
	id: "task-b",
	project_id: "project-1",
	title: "Review the report",
	task_number: 13,
};

const document: Document = {
	id: "doc-1",
	project_id: "project-1",
	title: "Project brief",
	content: [],
	position: 0,
	created_at: "2026-09-23T00:00:00Z",
	updated_at: "2026-09-23T00:00:00Z",
	visibility: "private",
	owner_user_id: "owner-1",
	access_version: 3,
	task_ids: [],
};

function Wrapper({
	children,
	client,
}: {
	children: ReactNode;
	client?: QueryClient;
}) {
	return (
		<QueryClientProvider
			client={
				client ??
				new QueryClient({
					defaultOptions: {
						queries: { retry: false },
						mutations: { retry: false },
					},
				})
			}
		>
			{children}
		</QueryClientProvider>
	);
}

describe("DocumentAccessPanel", () => {
	beforeEach(() => {
		vi.clearAllMocks();
		mockSearch.search = "";
		mockSearch.isSearching = false;
		mockSearch.isLoading = false;
		mockSearch.isError = false;
		mockSearch.results = [];
	});

	it("saves a task-scoped audience with the current access version", async () => {
		mockUpdateDocumentAccess.mockResolvedValue({
			id: "doc-1",
			visibility: "task",
			access_version: 4,
			task_ids: ["task-a"],
		});
		const user = userEvent.setup();

		render(
			<Wrapper>
				<DocumentAccessPanel
					projectId="project-1"
					document={document}
					canManage
				/>
			</Wrapper>,
		);

		await user.click(screen.getByRole("button", { name: /manage access/i }));
		await user.selectOptions(screen.getByLabelText(/visibility/i), "task");
		await user.click(await screen.findByRole("checkbox", { name: /#12/i }));
		await user.click(screen.getByRole("button", { name: /save/i }));

		await waitFor(() => {
			expect(mockUpdateDocumentAccess).toHaveBeenCalledWith(
				"project-1",
				"doc-1",
				{
					expected_access_version: 3,
					visibility: "task",
					task_ids: ["task-a"],
				},
			);
		});
	});

	it("keeps the local selection when the server reports an old-version conflict", async () => {
		mockUpdateDocumentAccess.mockRejectedValue({
			response: {
				status: 409,
				data: { error_code: "DOCUMENT_ACCESS_CONFLICT" },
			},
		});
		const user = userEvent.setup();

		render(
			<Wrapper>
				<DocumentAccessPanel
					projectId="project-1"
					document={document}
					canManage
				/>
			</Wrapper>,
		);

		await user.click(screen.getByRole("button", { name: /manage access/i }));
		await user.selectOptions(screen.getByLabelText(/visibility/i), "task");
		const taskCheckbox = await screen.findByRole("checkbox", { name: /#12/i });
		await user.click(taskCheckbox);
		await user.click(screen.getByRole("button", { name: /save/i }));

		await waitFor(() => {
			expect(
				screen.getByRole("button", { name: /refresh/i }),
			).toBeInTheDocument();
		});
		expect(taskCheckbox).toBeChecked();
		expect(screen.getByLabelText(/visibility/i)).toHaveValue("task");
	});

	it("shows a read-only source reference without an access control", async () => {
		const sourceDocument: Document = {
			...document,
			visibility: "task",
			task_ids: ["task-a"],
			source_document_id: "source-1",
			source_project_id: "source-project",
			source_title: "Original brief",
			source_updated_at: "2026-09-22T12:00:00Z",
		};

		render(
			<Wrapper>
				<DocumentAccessPanel
					projectId="project-1"
					document={sourceDocument}
					canManage={false}
				/>
			</Wrapper>,
		);

		expect(
			screen.queryByRole("button", { name: /manage access/i }),
		).not.toBeInTheDocument();
		expect(
			screen.getByRole("link", { name: "Original brief" }),
		).toHaveAttribute("href", "/projects/source-project/docs/source-1");
		expect(
			screen.getByText(/sharing this reference does not expand/i),
		).toBeInTheDocument();
	});

	it("invalidates all document data after changing its access scope", async () => {
		mockUpdateDocumentAccess.mockResolvedValue({
			id: "doc-1",
			visibility: "project",
			access_version: 4,
			task_ids: [],
		});
		const client = new QueryClient({
			defaultOptions: {
				queries: { retry: false },
				mutations: { retry: false },
			},
		});
		const invalidateQueries = vi.spyOn(client, "invalidateQueries");
		const user = userEvent.setup();

		render(
			<Wrapper client={client}>
				<DocumentAccessPanel
					projectId="project-1"
					document={document}
					canManage
				/>
			</Wrapper>,
		);

		await user.click(screen.getByRole("button", { name: /manage access/i }));
		await user.selectOptions(screen.getByLabelText(/visibility/i), "project");
		await user.click(screen.getByRole("button", { name: /save/i }));

		await waitFor(() => {
			expect(invalidateQueries).toHaveBeenCalledWith({
				queryKey: docQueryKeys.all("project-1"),
			});
		});
	});

	it("rebases only the access version after an explicit conflict refresh", async () => {
		mockUpdateDocumentAccess
			.mockRejectedValueOnce({
				response: {
					status: 409,
					data: { error_code: "DOCUMENT_ACCESS_CONFLICT" },
				},
			})
			.mockResolvedValueOnce({
				id: "doc-1",
				visibility: "task",
				access_version: 9,
				task_ids: ["task-a"],
			});
		mockGetDocument.mockResolvedValue({ ...document, access_version: 8 });
		const user = userEvent.setup();

		render(
			<Wrapper>
				<DocumentAccessPanel
					projectId="project-1"
					document={document}
					canManage
				/>
			</Wrapper>,
		);

		await user.click(screen.getByRole("button", { name: /manage access/i }));
		await user.selectOptions(screen.getByLabelText(/visibility/i), "task");
		const taskCheckbox = await screen.findByRole("checkbox", { name: /#12/i });
		await user.click(taskCheckbox);
		await user.click(screen.getByRole("button", { name: /save/i }));
		await user.click(await screen.findByRole("button", { name: /refresh/i }));
		await waitFor(() =>
			expect(screen.getByRole("status")).toHaveTextContent(/refreshed/i),
		);
		expect(taskCheckbox).toBeChecked();
		await user.click(screen.getByRole("button", { name: /save/i }));

		await waitFor(() => {
			expect(mockUpdateDocumentAccess).toHaveBeenLastCalledWith(
				"project-1",
				"doc-1",
				expect.objectContaining({
					expected_access_version: 8,
					task_ids: ["task-a"],
				}),
			);
		});
	});

	it("shows a recoverable error when task search fails", async () => {
		mockSearch.isSearching = true;
		mockSearch.isError = true;
		mockSearch.search = "report";
		const user = userEvent.setup();

		render(
			<Wrapper>
				<DocumentAccessPanel
					projectId="project-1"
					document={document}
					canManage
				/>
			</Wrapper>,
		);

		await user.click(screen.getByRole("button", { name: /manage access/i }));
		await user.selectOptions(screen.getByLabelText(/visibility/i), "task");
		expect(
			screen.getByText(/could not load or save access/i),
		).toBeInTheDocument();
	});

	it("opens the editor with keyboard activation", async () => {
		const user = userEvent.setup();
		render(
			<Wrapper>
				<DocumentAccessPanel
					projectId="project-1"
					document={document}
					canManage
				/>
			</Wrapper>,
		);

		const manage = screen.getByRole("button", { name: /manage access/i });
		manage.focus();
		await user.keyboard("{Enter}");

		expect(
			screen.getByRole("combobox", { name: /visibility/i }),
		).toBeInTheDocument();
	});
});
