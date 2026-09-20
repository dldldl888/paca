import { QueryCache, QueryClient } from "@tanstack/react-query";

export const queryClient = new QueryClient({
	defaultOptions: {
		queries: {
			retry(failures, error) {
				const status = (error as { response?: { status?: number } })?.response
					?.status;
				return status !== 403 && status !== 404 && failures < 3;
			},
		},
	},
	queryCache: new QueryCache({
		onError(error, query) {
			const status = (error as { response?: { status?: number } })?.response
				?.status;
			// A rejected refetch must not leave yesterday's authorized body as data.
			if (status === 403 || status === 404)
				query.setState({ data: undefined, dataUpdatedAt: 0 });
		},
	}),
});

export function reauthorizeQueries(): void {
	// Keep mounted components (and their unsaved private input) alive. Inactive
	// caches must be fetched again on navigation, including formerly fresh data.
	// Disabled observers include the live conversation tail; removal alone
	// would leave their last result mounted without an authorization refetch.
	void queryClient.resetQueries({
		type: "inactive",
		predicate: (query) => query.getObserversCount() > 0,
	});
	queryClient.removeQueries({
		predicate: (query) => query.getObserversCount() === 0,
	});
	void queryClient.cancelQueries().then(() => queryClient.invalidateQueries());
}
