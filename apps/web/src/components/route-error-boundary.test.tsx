import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { RouteErrorComponent } from "./route-error-boundary";

afterEach(cleanup);

describe("RouteErrorComponent", () => {
	it("renders an unknown thrown value without crashing the error boundary", () => {
		render(
			<RouteErrorComponent error={{ message: { internal: "private" } }} />,
		);
		expect(screen.getByText("Something went wrong")).toBeInTheDocument();
		expect(screen.getByRole("button", { name: "Retry" })).toBeEnabled();
		expect(screen.queryByText("private")).not.toBeInTheDocument();
	});

	it("preserves not-found handling for normal Error instances", () => {
		render(<RouteErrorComponent error={new Error("404 Not found")} />);
		expect(screen.getByText("Not found")).toBeInTheDocument();
		expect(screen.getByText("404 Not found")).toBeInTheDocument();
	});
});
