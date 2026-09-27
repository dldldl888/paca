import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ComponentType } from "react";
import { beforeEach, expect, it, vi } from "vitest";

import type { User } from "@/lib/admin-api";
import { apiClient } from "@/lib/api-client";
import { Route } from "@/routes/_authenticated/admin/users/index";

const UsersManagementPage = Route.options.component as ComponentType;
let users: User[];
let failSecondPage: boolean;

beforeEach(() => {
	vi.restoreAllMocks();
	failSecondPage = false;
	users = Array.from({ length: 40 }, (_, i) => ({
		id: `user-${i + 1}`,
		username: `employee_${i + 1}`,
		full_name: `Employee ${i + 1}`,
		role: "USER",
		must_change_password: true,
		created_at: "2026-09-20T00:00:00Z",
	}));
	vi.spyOn(apiClient.instance, "get").mockImplementation(
		async (url, config) => {
			let data: unknown;
			if (url === "/users/me/global-permissions") {
				data = {
					permissions: [
						"users.read",
						"users.write",
						"users.delete",
						"global_roles.read",
					],
				};
			} else if (url === "/users/me") {
				data = { ...users[0], id: "admin", username: "admin" };
			} else if (url === "/admin/global-roles") {
				data = [
					{
						id: "role-user",
						name: "USER",
						permissions: { "projects.read": true },
						created_at: "2026-09-20T00:00:00Z",
						updated_at: "2026-09-20T00:00:00Z",
					},
				];
			} else if (url === "/admin/users") {
				const { page = 1, page_size: pageSize = 20 } = (config?.params ??
					{}) as {
					page?: number;
					page_size?: number;
				};
				if (failSecondPage && page === 2)
					throw new Error("Network unavailable");
				data = {
					items: users.slice((page - 1) * pageSize, page * pageSize),
					total: users.length,
					page,
					page_size: pageSize,
				};
			} else throw new Error(`Unexpected request: ${url}`);
			return { data: { data } };
		},
	);
});

async function renderPage() {
	const client = new QueryClient({
		defaultOptions: { queries: { retry: false } },
	});
	await act(async () => {
		render(
			<QueryClientProvider client={client}>
				<UsersManagementPage />
			</QueryClientProvider>,
		);
	});
	return userEvent.setup();
}

it("lets an administrator reach users beyond the first 20 and return", async () => {
	const user = await renderPage();
	await screen.findByText("employee_20");
	expect(screen.queryByText("employee_21")).not.toBeInTheDocument();
	await screen.findByText("40 users in current list");
	expect(screen.getByRole("button", { name: "Previous page" })).toBeDisabled();
	expect(screen.getByText("Page 1 of 2")).toBeInTheDocument();
	await user.click(screen.getByRole("button", { name: "Next page" }));
	await screen.findByText("employee_40");
	expect(screen.queryByText("employee_1")).not.toBeInTheDocument();
	expect(screen.getByText("Page 2 of 2")).toBeInTheDocument();
	expect(screen.getByRole("button", { name: "Next page" })).toBeDisabled();
	await user.click(screen.getByRole("button", { name: "Previous page" }));
	await screen.findByText("employee_1");
	expect(screen.getByRole("button", { name: "Previous page" })).toBeDisabled();
});

it("keeps a way back when loading the second page fails", async () => {
	const user = await renderPage();
	await screen.findByText("employee_20");
	failSecondPage = true;
	await user.click(screen.getByRole("button", { name: "Next page" }));
	await screen.findByText("Failed to load users");
	expect(screen.getByRole("button", { name: "Next page" })).toBeDisabled();
	await user.click(screen.getByRole("button", { name: "Previous page" }));
	await screen.findByText("employee_1");
});

it("does not show partial role coverage when the full list fails", async () => {
	failSecondPage = true;
	await renderPage();
	await screen.findByText("employee_20");
	await screen.findByText(
		"Role coverage is unavailable until the full user list loads.",
	);
	expect(
		screen.queryByLabelText("Organization scope (read-only)"),
	).not.toBeInTheDocument();
});

it("returns to the remaining page after deleting the last user on page two", async () => {
	users = users.slice(0, 21);
	vi.spyOn(apiClient.instance, "delete").mockImplementation(async (url) => {
		if (url !== "/admin/users/user-21")
			throw new Error(`Unexpected deletion: ${url}`);
		users = users.filter((user) => user.id !== "user-21");
		return { data: {} };
	});
	const user = await renderPage();
	await screen.findByText("employee_20");
	await user.click(screen.getByRole("button", { name: "Next page" }));
	await screen.findByText("employee_21");
	await user.click(screen.getByTitle("Delete user"));
	await user.click(
		within(screen.getByRole("dialog")).getByRole("button", {
			name: "Delete user",
		}),
	);
	await screen.findByText("employee_20");
	expect(screen.queryByText("employee_21")).not.toBeInTheDocument();
	await waitFor(() =>
		expect(
			screen.queryByRole("button", { name: "Next page" }),
		).not.toBeInTheDocument(),
	);
});
