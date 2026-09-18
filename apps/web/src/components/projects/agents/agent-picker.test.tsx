import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
	act,
	render,
	renderHook,
	screen,
	waitFor,
} from "@testing-library/react";
import i18next from "i18next";
import type { PropsWithChildren } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { loadLanguage } from "@/i18n/config";
import { myPermissionsQueryOptions } from "@/lib/admin-api";
import { type Agent, chattableAgentsQueryOptions } from "@/lib/agent-api";
import {
	AgentPickerContext,
	AgentPickerInline,
	getAgentDisplayName,
	useGlobalAgentPicker,
} from "./agent-picker";
import { NewConversationThread } from "./new-conversation-thread";

vi.mock("@tanstack/react-router", async () => ({
	...(await vi.importActual<typeof import("@tanstack/react-router")>(
		"@tanstack/react-router",
	)),
	useNavigate: () => vi.fn(),
	useParams: () => ({}),
}));

const assistant: Agent = {
	id: "hermes",
	name: "公司助手 Hermes",
	handle: "hermes",
	agent_scope: "global",
	agent_type: "acp",
	acp_provider: "custom",
	llm_provider: "",
	llm_model: "",
	llm_base_url: "",
	has_acp_bridge_token: true,
	has_mcp_api_key: false,
	system_prompt: "",
	git_committer_name: "",
	git_committer_email: "",
	docker_enabled: false,
	parallelism_limit: 1,
	created_at: "2026-09-17T00:00:00Z",
	updated_at: "2026-09-17T00:00:00Z",
};
const other: Agent = {
	...assistant,
	id: "other",
	name: "Other assistant",
	handle: "other",
};

function harness(agents: Agent[]) {
	const client = new QueryClient({
		defaultOptions: { queries: { staleTime: Infinity, retry: false } },
	});
	client.setQueryData(chattableAgentsQueryOptions.queryKey, agents);
	client.setQueryData(myPermissionsQueryOptions.queryKey, []);
	return ({ children }: PropsWithChildren) => (
		<QueryClientProvider client={client}>{children}</QueryClientProvider>
	);
}

describe("useGlobalAgentPicker", () => {
	beforeEach(async () => {
		await loadLanguage("zh-CN");
		await i18next.changeLanguage("zh-CN");
	});

	it("defaults to the company ACP Hermes among other agents without a project", async () => {
		const { result } = renderHook(() => useGlobalAgentPicker(), {
			wrapper: harness([other, assistant]),
		});
		await waitFor(() => expect(result.current.agentId).toBe("hermes"));
		act(() => result.current.setAgentId("other"));
		expect(result.current.agentId).toBe("other");
	});

	it("labels only the known global Hermes agent as Xiaopai", () => {
		expect(getAgentDisplayName(assistant)).toBe("小派");
		expect(getAgentDisplayName(other)).toBe("Other assistant");
		expect(getAgentDisplayName({ ...assistant, agent_scope: "project" })).toBe(
			"公司助手 Hermes",
		);
	});

	it.each([
		["a lone unrelated agent", other],
		[
			"a similarly named non-ACP agent",
			{ ...assistant, agent_type: "llm" } as Agent,
		],
		[
			"a project-scoped Hermes",
			{
				...assistant,
				agent_scope: "project",
				project_id: "project-1",
			} as Agent,
		],
	])("requires explicit selection for %s", (_label, agent) => {
		const { result } = renderHook(() => useGlobalAgentPicker(), {
			wrapper: harness([agent]),
		});
		expect(result.current.agentId).toBe("");
	});

	it("shows an administrator contact message instead of requiring employees to create an agent", () => {
		function Picker() {
			const { pickerState } = useGlobalAgentPicker();
			return (
				<AgentPickerContext.Provider value={pickerState}>
					<AgentPickerInline />
				</AgentPickerContext.Provider>
			);
		}
		render(<Picker />, { wrapper: harness([]) });
		expect(screen.getByText(/联系管理员/)).toBeInTheDocument();
		expect(
			screen.queryByRole("button", { name: /新建智能体/ }),
		).not.toBeInTheDocument();
	});

	it("renders the company assistant composer without project or task context", () => {
		vi.stubGlobal(
			"ResizeObserver",
			class {
				observe() {}
				unobserve() {}
				disconnect() {}
			},
		);
		render(<NewConversationThread companyAssistant />, {
			wrapper: harness([assistant]),
		});
		expect(screen.getByRole("heading", { name: "小派" })).toBeInTheDocument();
		expect(screen.getByRole("textbox")).toBeEnabled();
	});
});
