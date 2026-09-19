import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import i18next from "i18next";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { loadLanguage } from "@/i18n/config";
import { myPermissionsQueryOptions } from "@/lib/admin-api";
import {
	type AgentConversation,
	chattableAgentsQueryOptions,
	globalConversationQueryOptions,
} from "@/lib/agent-api";
import { ConversationView } from "./conversation-view";

const { get } = vi.hoisted(() => ({ get: vi.fn() }));
vi.mock("@/lib/api-client", () => ({ apiClient: { instance: { get } } }));
vi.mock("@/lib/socket-client", () => ({
	connectSocket: () => ({ on() {}, off() {} }),
}));
vi.mock("@tanstack/react-router", async () => ({
	...(await vi.importActual<typeof import("@tanstack/react-router")>(
		"@tanstack/react-router",
	)),
	useParams: () => ({}),
}));
beforeEach(async () => {
	await loadLanguage("zh-CN");
	await i18next.changeLanguage("zh-CN");
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
describe("ConversationView", () => {
	it("renders Xiaopai sources and safe errors without internal prompt context", async () => {
		const conversation: AgentConversation = {
			id: "conv-source",
			agent_id: "hermes",
			actor_user_id: "employee",
			trigger_type: "chat_message",
			chat_session_id: "chat",
			status: "failed",
			error_message: "小派暂时离线，请稍后重试。",
			iteration_count: 0,
			input_tokens: 0,
			output_tokens: 0,
			total_tokens: 0,
			created_at: "2026-09-18T00:00:00Z",
			updated_at: "2026-09-18T00:00:01Z",
		};
		const events = [
			{
				id: "question",
				conversation_id: conversation.id,
				event_index: 0,
				event_type: "user_message",
				event_source: "user",
				payload: {
					content: { type: "text", text: "如何报销？" },
					enhanced_prompt: "INTERNAL_PRIVATE_GOAL",
					system_prompt: "INTERNAL_SYSTEM_PROMPT",
				},
				created_at: "2026-09-18T00:00:00Z",
			},
			{
				id: "answer",
				conversation_id: conversation.id,
				event_index: 1,
				event_type: "agent_message_chunk",
				event_source: "agent",
				payload: {
					content: {
						type: "text",
						text: "请提交报销单。\n\n来源：[报销制度](/projects/company/docs/policy)\n\n更新时间：2026-09-18",
					},
				},
				created_at: "2026-09-18T00:00:01Z",
			},
		];
		get.mockImplementation(async (path: string) => {
			if (!path.endsWith("/events")) throw new Error(`Unexpected GET ${path}`);
			return {
				data: {
					success: true,
					data: {
						items: events,
						total: events.length,
						next_cursor: null,
						prev_cursor: null,
					},
				},
			};
		});
		const client = new QueryClient({
			defaultOptions: { queries: { retry: false, staleTime: Infinity } },
		});
		client.setQueryData(myPermissionsQueryOptions.queryKey, []);
		client.setQueryData(chattableAgentsQueryOptions.queryKey, [
			{
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
				created_at: "2026-09-18T00:00:00Z",
				updated_at: "2026-09-18T00:00:00Z",
			},
		]);
		client.setQueryData(
			globalConversationQueryOptions(conversation.id).queryKey,
			conversation,
		);
		const view = render(
			<QueryClientProvider client={client}>
				<ConversationView conversationId={conversation.id} />
			</QueryClientProvider>,
		);
		expect(await screen.findByText("如何报销？")).toBeInTheDocument();
		expect(
			await screen.findByRole("link", { name: "报销制度" }),
		).toHaveAttribute("href", "/projects/company/docs/policy");
		expect(screen.getByText("更新时间：2026-09-18")).toBeInTheDocument();
		expect(
			screen.getByText("暂时无法完成回复，请重试。若仍失败，请联系管理员。"),
		).toBeInTheDocument();
		expect(
			screen.queryByText(
				/INTERNAL_PRIVATE_GOAL|INTERNAL_SYSTEM_PROMPT|TypeError|stack trace/,
			),
		).not.toBeInTheDocument();
		expect(screen.getByRole("textbox")).toBeEnabled();
		view.unmount();
		client.setQueryData(
			globalConversationQueryOptions(conversation.id).queryKey,
			{
				...conversation,
				error_message:
					"TypeError: INTERNAL_SYSTEM_PROMPT at handler.go:123\n    at dispatch (/srv/private.ts:42)",
			},
		);
		render(
			<QueryClientProvider client={client}>
				<ConversationView conversationId={conversation.id} />
			</QueryClientProvider>,
		);
		expect(await screen.findByText("如何报销？")).toBeInTheDocument();
		expect(
			screen.queryByText(
				/TypeError|INTERNAL_SYSTEM_PROMPT|handler.go|private.ts/,
			),
		).not.toBeInTheDocument();
		expect(screen.getByText(/稍后|重试|暂时/)).toBeInTheDocument();
	});
});
