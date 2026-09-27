import { describe, expect, it, vi } from "vitest";

import { Route } from "./index";

describe("home route loader", () => {
	it("keeps assigned task failures in the widget query state", async () => {
		const loader = (
			Route as unknown as {
				options: {
					loader: (args: {
						context: { queryClient: unknown };
					}) => Promise<void>;
				};
			}
		).options.loader;
		const queryClient = {
			ensureQueryData: vi.fn().mockResolvedValue({}),
			ensureInfiniteQueryData: vi
				.fn()
				.mockRejectedValue(new Error("assigned tasks unavailable")),
			prefetchInfiniteQuery: vi.fn().mockResolvedValue(undefined),
		};

		await expect(loader({ context: { queryClient } })).resolves.toBeUndefined();
		expect(queryClient.prefetchInfiniteQuery).toHaveBeenCalledTimes(1);
	});
});
