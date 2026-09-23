import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { OrganizationScopeSummary } from "./OrganizationScopeSummary";

describe("OrganizationScopeSummary", () => {
	it("shows global role impact without presenting it as a project role", () => {
		render(
			<OrganizationScopeSummary
				users={[
					{
						id: "u1",
						username: "owner",
						full_name: "Owner",
						role: "BUSINESS_OWNER",
						must_change_password: false,
						created_at: "2026-09-23T00:00:00Z",
					},
				]}
				totalUsers={1}
				roles={[
					{
						id: "r1",
						name: "BUSINESS_OWNER",
						permissions: { "users.read": true, "projects.create": true },
						created_at: "2026-09-23T00:00:00Z",
						updated_at: "2026-09-23T00:00:00Z",
					},
				]}
			/>,
		);

		expect(
			screen.getByRole("region", { name: "Organization scope (read-only)" }),
		).toBeInTheDocument();
		expect(screen.getByText("BUSINESS_OWNER")).toBeInTheDocument();
		expect(screen.getByText("1 user in current list")).toBeInTheDocument();
		expect(screen.getByText("users.read, projects.create")).toBeInTheDocument();
		expect(
			screen.getByText(
				"Project roles remain managed within each project team.",
			),
		).toBeInTheDocument();
	});

	it("states that formal department bindings are not configured", () => {
		render(<OrganizationScopeSummary users={[]} totalUsers={0} roles={[]} />);

		expect(
			screen.getByText("Formal department bindings are not configured"),
		).toBeInTheDocument();
		expect(
			screen.getByText(
				/Department scope is never inferred from names or role labels/,
			),
		).toBeInTheDocument();
	});
});
