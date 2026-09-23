import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import i18next from "i18next";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { AIChatFloat } from "@/components/projects/ai-chat-float";
import { GlobalAIChatFloat } from "@/components/projects/ai-chat-float-global";
import { myPermissionsQueryOptions } from "@/lib/admin-api";
import {
	type Agent,
	type AgentConversation,
	type AgentType,
	agentQueryOptions,
	agentsQueryOptions,
	chattableAgentsQueryOptions,
	conversationQueryOptions,
	globalConversationQueryOptions,
} from "@/lib/agent-api";
import { myProjectPermissionsQueryOptions } from "@/lib/project-api";
import { ConversationView } from "./conversation-view";

const { get, post } = vi.hoisted(() => ({ get: vi.fn(), post: vi.fn() }));
vi.mock("@/lib/api-client", () => ({ apiClient: { instance: { get, post } } }));
vi.mock("@/lib/socket-client", () => ({
	connectSocket: () => ({ on() {}, off() {} }),
}));
vi.mock("@tanstack/react-router", async () => ({
	...(await vi.importActual<typeof import("@tanstack/react-router")>(
		"@tanstack/react-router",
	)),
	useParams: () => ({}),
}));

const baseAgent: Agent = {
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

describe("composer cancellation", () => {
	beforeEach(async () => {
		vi.clearAllMocks();
		await i18next.changeLanguage("en");
		vi.stubGlobal(
			"ResizeObserver",
			class {
				observe() {}
				unobserve() {}
				disconnect() {}
			},
		);
		Object.defineProperty(HTMLElement.prototype, "scrollTo", {
			configurable: true,
			value: vi.fn(),
		});
	});

	it.each([
		[
			"global conversation",
			undefined,
			"acp",
			false,
			"/agents/conversations/conv-1/stop",
		],
		[
			"project conversation",
			"project-1",
			"acp",
			false,
			"/projects/project-1/conversations/conv-1/stop",
		],
		[
			"global LLM conversation",
			undefined,
			"llm",
			false,
			"/agents/conversations/conv-1/pause",
		],
		[
			"project LLM conversation",
			"project-1",
			"llm",
			false,
			"/projects/project-1/conversations/conv-1/pause",
		],
		[
			"global floating chat",
			undefined,
			"acp",
			true,
			"/agents/conversations/conv-1/stop",
		],
		[
			"project floating chat",
			"project-1",
			"acp",
			true,
			"/projects/project-1/conversations/conv-1/stop",
		],
		[
			"delayed global agent",
			undefined,
			"acp",
			false,
			"/agents/conversations/conv-1/stop",
			true,
		],
		[
			"delayed project agent",
			"project-1",
			"acp",
			false,
			"/projects/project-1/conversations/conv-1/stop",
			true,
		],
		[
			"delayed project floating agent",
			"project-1",
			"acp",
			true,
			"/projects/project-1/conversations/conv-1/stop",
			true,
		],
	] as const)(
		"clicking Stop generating uses the correct control in %s",
		async (_label, projectId, agentType, floating, controlPath, delayAgent: boolean = false) => {
			const agent: Agent = {
				...baseAgent,
				agent_type: agentType as AgentType,
				agent_scope: projectId ? "project" : "global",
				project_id: projectId ?? null,
			};
			let conversation: AgentConversation = {
				id: "conv-1",
				agent_id: agent.id,
				project_id: projectId ?? null,
				actor_user_id: "employee-1",
				trigger_type: "chat_message",
				chat_session_id: "chat-1",
				status: "running",
				iteration_count: 0,
				input_tokens: 0,
				output_tokens: 0,
				total_tokens: 0,
				created_at: "2026-09-17T00:00:00Z",
				updated_at: "2026-09-17T00:00:00Z",
			};
			const envelope = (data: unknown) => ({ data: { success: true, data } });
			let pauseRequested = false;
			let refetchStarted = false;
			let releaseRefetch = () => {};
			const refetchGate = new Promise<void>((resolve) => {
				releaseRefetch = resolve;
			});
			let queryClient: QueryClient;
			let releaseAgent = () => {};
			const agentResponse = new Promise<ReturnType<typeof envelope>>(
				(resolve) => {
					releaseAgent = () =>
						resolve(envelope(projectId ? agent : { items: [agent] }));
				},
			);
			get.mockImplementation(async (path: string) => {
				if (path === "/agents" || path.endsWith("/agents/hermes"))
					return agentResponse;
				if (path.endsWith("/events"))
					return envelope({
						items: [],
						total: 0,
						next_cursor: null,
						prev_cursor: null,
					});
				if (path.endsWith("/conv-1")) {
					if (pauseRequested && !refetchStarted) {
						refetchStarted = true;
						await refetchGate;
					}
					return envelope(conversation);
				}
				throw new Error(`Unexpected GET ${path}`);
			});
			post.mockImplementation(async (path: string) => {
				if (path.endsWith("/chat-sessions"))
					return envelope({
						session: {
							id: "chat-1",
							agent_id: agent.id,
							project_id: projectId ?? null,
							member_id: projectId ? "member-1" : null,
							actor_user_id: projectId ? null : "employee-1",
							title: "Hello",
							last_message_at: "2026-09-17T00:00:00Z",
							created_at: "2026-09-17T00:00:00Z",
							updated_at: "2026-09-17T00:00:00Z",
						},
						conversation,
					});
				if (path.endsWith("/stop"))
					conversation = { ...conversation, status: "stopped" };
				if (path.endsWith("/pause") && agentType === "llm")
					pauseRequested = true;
				return envelope(conversation);
			});
			const client = new QueryClient({
				defaultOptions: { queries: { retry: false, staleTime: Infinity } },
			});
			queryClient = client;
			client.setQueryData(myPermissionsQueryOptions.queryKey, []);
			if (!delayAgent)
				client.setQueryData(chattableAgentsQueryOptions.queryKey, [agent]);
			const conversationOptions = projectId
				? conversationQueryOptions(projectId, conversation.id)
				: globalConversationQueryOptions(conversation.id);
			client.setQueryData(conversationOptions.queryKey, conversation);
			if (projectId) {
				client.setQueryData(agentsQueryOptions(projectId).queryKey, [agent]);
				if (!delayAgent)
					client.setQueryData(
						agentQueryOptions(projectId, agent.id).queryKey,
						agent,
					);
				client.setQueryData(
					myProjectPermissionsQueryOptions(projectId).queryKey,
					{ "conversations.write": true },
				);
			}
			render(
				<QueryClientProvider client={client}>
					{floating ? (
						projectId ? (
							<AIChatFloat projectId={projectId} />
						) : (
							<GlobalAIChatFloat />
						)
					) : (
						<ConversationView
							projectId={projectId}
							conversationId={conversation.id}
						/>
					)}
				</QueryClientProvider>,
			);
			const user = userEvent.setup();
			if (floating) {
				await user.click(
					screen.getByRole("button", { name: /Chat with AI agent/i }),
				);
				await user.type(screen.getByRole("textbox"), "Hello");
				await user.click(screen.getByRole("button", { name: "Send message" }));
			}
			await user.click(
				await screen.findByRole("button", { name: "Stop generating" }),
			);
			if (delayAgent) {
				expect(post.mock.calls.some(([path]) => path.endsWith("/pause"))).toBe(
					false,
				);
				releaseAgent();
			}
			await waitFor(() => expect(post).toHaveBeenCalledWith(controlPath));
			if (agentType === "llm") {
				await waitFor(() => expect(refetchStarted).toBe(true));
				releaseRefetch();
				await waitFor(() =>
					expect(
						screen.queryByRole("button", { name: "Stop generating" }),
					).not.toBeInTheDocument(),
				);
				const key = projectId
					? conversationQueryOptions(projectId, conversation.id).queryKey
					: globalConversationQueryOptions(conversation.id).queryKey;
				queryClient.setQueryData(key, { ...conversation, status: "paused" });
			}
			await waitFor(() =>
				expect(
					screen.queryByRole("button", { name: "Stop generating" }),
				).not.toBeInTheDocument(),
			);
			expect(screen.getByRole("textbox")).toBeEnabled();
		},
	);
});
