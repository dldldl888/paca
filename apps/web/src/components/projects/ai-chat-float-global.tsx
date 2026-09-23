import {
	type AppendMessage,
	AssistantRuntimeProvider,
	useExternalStoreRuntime,
} from "@assistant-ui/react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { AlertTriangle, Bot, Plus, X } from "lucide-react";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { Thread } from "@/components/assistant-ui/thread";
import {
	AgentPickerContext,
	AgentPickerInline,
	useGlobalAgentPicker,
} from "@/components/projects/agents/agent-picker";
import { Button } from "@/components/ui/button";
import { useGlobalAgentRealtime } from "@/hooks/use-global-agent-realtime";
import {
	type AgentConversation,
	CONVERSATION_HEARTBEAT_INTERVAL_MS,
	chattableAgentsQueryOptions,
	globalConversationEventsQueryOptions,
	globalConversationQueryOptions,
	heartbeatGlobalConversation,
	pauseGlobalConversation,
	sendGlobalChatMessage,
	sendGlobalConversationMessage,
	startGlobalChatSession,
	stopGlobalConversation,
} from "@/lib/agent-api";
import { useContextInjectionStore } from "@/lib/context-injection-store";
import { cn } from "@/lib/utils";
import { ConversationErrorBox } from "./agents/conversation-error-box";
import {
	canReplyToConversation,
	eventsToThreadMessages,
	extractTextOnlyContent,
	isEnvironmentReady,
} from "./agents/conversation-to-thread-messages";

// Global sibling of ai-chat-float.tsx's AIChatFloat — same floating-panel UX,
// but for chatting with a global agent from the home page / admin pages,
// where there is no projectId to scope any of this against. Kept as a
// separate component rather than threading an optional projectId through
// AIChatFloat: nearly every line there reads or is keyed on projectId (API
// calls, query keys, the agent picker), so branching it internally would
// obscure the one existing, well-tested project-scoped component for the
// sake of a code path that behaves quite differently underneath (different
// API endpoints, different realtime room, different permission model for
// the agent picker's empty state). See lib/agent-api.ts's global-* functions
// and hooks/use-global-agent-realtime.ts for the underlying plumbing.

const EMPTY_EVENTS: never[] = [];

const THREAD_COMPONENTS = {
	ComposerStart: AgentPickerInline,
	Welcome: () => null,
};

function FloatingChatFailedBanner() {
	const { t } = useTranslation("projects");
	return (
		<div className="flex flex-col items-center gap-3 px-6 py-8 text-center">
			<div className="flex size-10 items-center justify-center rounded-full bg-destructive/10">
				<AlertTriangle className="size-5 text-destructive" />
			</div>
			<div className="space-y-1">
				<p className="text-sm font-medium text-destructive">
					{t("agents.conversationView.failed")}
				</p>
				<p className="text-xs text-muted-foreground wrap-break-word">
					{t("agents.conversationView.safeError")}
				</p>
			</div>
		</div>
	);
}

export function GlobalAIChatFloat() {
	const { t } = useTranslation("projects");
	const [open, setOpen] = useState(false);
	const [conversationId, setConversationId] = useState<string | null>(null);
	const [cancelRequested, setCancelRequested] = useState(false);
	const cancelPendingRef = useRef(false);
	const [isSubmitting, setIsSubmitting] = useState(false);
	const qc = useQueryClient();

	useGlobalAgentRealtime();

	const { agentId, pickerState } = useGlobalAgentPicker({
		disabled: !!conversationId || isSubmitting,
	});

	const { data: conversation } = useQuery({
		...globalConversationQueryOptions(conversationId ?? ""),
		enabled: !!conversationId,
	});
	const { data: events = EMPTY_EVENTS } = useQuery({
		...globalConversationEventsQueryOptions(conversationId ?? ""),
		enabled: !!conversationId,
	});
	// GET /agents (chattableAgentsQueryOptions) rather than the admin-only
	// GET /admin/agents/:id — any user chatting with a global agent may not
	// have agents.read, but this list is deliberately unrestricted (see
	// lib/agent-api.ts's listChattableAgents).
	const { data: agents = [] } = useQuery(chattableAgentsQueryOptions);
	const agent = agents.find((a) => a.id === conversation?.agent_id);
	const isACP =
		conversation?.agent_type === "acp" || agent?.agent_type === "acp";
	const environmentReady = isEnvironmentReady(isACP, events);

	const isRunning =
		conversation?.status === "queued" || conversation?.status === "running";
	useEffect(() => {
		if (!isRunning && !cancelPendingRef.current) {
			setCancelRequested(false);
			cancelPendingRef.current = false;
		}
	}, [isRunning]);
	const isTerminal =
		conversation?.status === "finished" ||
		conversation?.status === "failed" ||
		conversation?.status === "stopped";

	const messages = useMemo(
		() => (conversationId ? eventsToThreadMessages(events, isRunning) : []),
		[events, isRunning, conversationId],
	);

	const invalidate = useCallback(
		(id: string | null = conversationId) => {
			if (id) {
				qc.invalidateQueries({
					queryKey: ["global-chat", "conversations", id],
				});
			}
			qc.invalidateQueries({
				queryKey: ["global-chat", "conversations"],
			});
		},
		[conversationId, qc],
	);

	const onNew = useCallback(
		async (message: AppendMessage) => {
			const text = extractTextOnlyContent(message);
			if (text === null) {
				throw new Error(t("agents.conversationView.textOnlyMessage"));
			}
			cancelPendingRef.current = false;
			setCancelRequested(false);
			// Snapshot now (not read again after any await) so a badge staged
			// mid-send can't sneak into this message or get cleared under it.
			const contextItems = useContextInjectionStore.getState().items;

			setIsSubmitting(true);
			try {
				if (!conversationId) {
					const selectedAgentId = agentId;
					if (!selectedAgentId) throw new Error(t("aiChat.selectAgentFirst"));
					const result = await startGlobalChatSession(selectedAgentId, {
						message: text,
						contextItems,
					});
					useContextInjectionStore.getState().clear();
					qc.setQueryData(
						globalConversationQueryOptions(result.conversation.id).queryKey,
						result.conversation,
					);
					setConversationId(result.conversation.id);
					void qc.invalidateQueries({
						queryKey: ["global-chat", "conversations"],
					});
					return;
				}

				if (!conversation) {
					throw new Error(t("agents.conversationView.conversationEnded"));
				}
				if (!conversation.chat_session_id) {
					// ACP (see canReplyToConversation's own doc comment) — reply in
					// place on the same conversation_id rather than through a chat
					// session.
					await sendGlobalConversationMessage(
						conversation.id,
						text,
						contextItems,
					);
					useContextInjectionStore.getState().clear();
					invalidate();
					return;
				}
				const result = await sendGlobalChatMessage(
					conversation.chat_session_id,
					{
						message: text,
						contextItems,
					},
				);
				useContextInjectionStore.getState().clear();
				if (result.id !== conversationId) {
					qc.setQueryData(
						globalConversationQueryOptions(result.id).queryKey,
						result,
					);
					setConversationId(result.id);
				}
				invalidate(result.id);
			} finally {
				setIsSubmitting(false);
			}
		},
		[agentId, conversation, conversationId, invalidate, qc, t],
	);

	const onCancel = useCallback(async () => {
		if (!conversationId || !conversation || cancelPendingRef.current) return;
		cancelPendingRef.current = true;
		setCancelRequested(true);
		try {
			const agentType =
				conversation.agent_type ??
				agent?.agent_type ??
				(await qc.ensureQueryData(chattableAgentsQueryOptions)).find(
					(a) => a.id === conversation.agent_id,
				)?.agent_type;
			if (!agentType) throw new Error(t("aiChat.noAgentsAvailable"));
			await (agentType === "acp"
				? stopGlobalConversation
				: pauseGlobalConversation)(conversationId);
			// Hide the control immediately, but keep the cancel lock until a
			// later send or conversation change. Pause is asynchronous and a
			// refetch can briefly report running again.
			const nextStatus = agentType === "acp" ? "stopped" : "paused";
			qc.setQueryData<AgentConversation>(
				globalConversationQueryOptions(conversationId).queryKey,
				(current) => (current ? { ...current, status: nextStatus } : current),
			);
			invalidate();
		} catch (error) {
			setCancelRequested(false);
			cancelPendingRef.current = false;
			throw error;
		}
	}, [agent, conversation, conversationId, invalidate, qc, t]);

	// !conversationId: no conversation yet, so there's nothing to be blocked
	// on — the composer is for starting a brand new one. See
	// canReplyToConversation's own doc comment for every other case.
	const canReply =
		!conversationId || canReplyToConversation(conversation, isACP);

	const showFailedBanner =
		!!conversationId &&
		isTerminal &&
		conversation?.status === "failed" &&
		messages.length === 0 &&
		!canReply;

	const runtimeStore = useMemo(
		() => ({
			messages,
			isRunning: isRunning && !cancelRequested,
			convertMessage: (m: (typeof messages)[number]) => m,
			onNew,
			onCancel,
			isDisabled: !canReply,
			isSendDisabled: isSubmitting || (!conversationId && !agentId),
		}),
		[
			messages,
			isRunning,
			cancelRequested,
			onCancel,
			onNew,
			canReply,
			conversationId,
			isSubmitting,
			agentId,
		],
	);
	const runtime = useExternalStoreRuntime(runtimeStore);

	const endConversation = useCallback(
		(id: string) => {
			const cached = qc.getQueryData<AgentConversation>(
				globalConversationQueryOptions(id).queryKey,
			);
			if (cached?.status === "queued" || cached?.status === "running") return;
			void stopGlobalConversation(id).catch(() => {});
		},
		[qc],
	);

	function handleNewConversation() {
		if (conversationId) endConversation(conversationId);
		setConversationId(null);
	}

	function handleToggleOpen() {
		setOpen((o) => !o);
	}

	useEffect(() => {
		if (!conversationId || isTerminal || isACP) return;
		const id = conversationId;
		void heartbeatGlobalConversation(id).catch(() => {});
		const interval = setInterval(() => {
			void heartbeatGlobalConversation(id).catch(() => {});
		}, CONVERSATION_HEARTBEAT_INTERVAL_MS);
		return () => clearInterval(interval);
	}, [conversationId, isTerminal, isACP]);

	return (
		<>
			{/* Floating trigger button */}
			<button
				type="button"
				aria-label={t("aiChat.chatWithAgent")}
				onClick={handleToggleOpen}
				className={cn(
					// z-70: must stay above the task-detail dialog (z-50, with its
					// own backdrop-blur) so the float stays clickable and unblurred
					// while that dialog is open, and above its nested field dialog
					// (z-60) too.
					"fixed bottom-6 right-6 z-70 flex size-12 items-center justify-center rounded-full shadow-lg transition-all hover:scale-105",
					open
						? "bg-muted text-foreground border border-border"
						: "bg-primary text-primary-foreground hover:bg-primary/90",
				)}
			>
				{open ? <X className="size-5" /> : <Bot className="size-5" />}
			</button>

			{/* Chat panel */}
			{open && (
				<div
					className={cn(
						"fixed bottom-20 right-6 z-70 flex w-95 flex-col overflow-hidden rounded-2xl border border-border/60 bg-background shadow-2xl",
						conversationId ? "h-150" : "max-h-150",
					)}
				>
					{/* Panel header */}
					<div className="flex shrink-0 items-center justify-between border-b border-border/40 bg-muted/30 px-4 py-3">
						<div className="flex items-center gap-2">
							<Bot className="size-4 text-primary" />
							<span className="text-sm font-semibold">
								{t("aiChat.chatWithAgent")}
							</span>
						</div>
						{conversationId && (
							<Button
								size="sm"
								variant="outline"
								className="h-7 gap-1.5 text-xs"
								onClick={handleNewConversation}
							>
								<Plus className="size-3" />
								{t("aiChat.newConversation")}
							</Button>
						)}
					</div>

					<div className="min-h-0 flex-1 overflow-y-auto">
						{showFailedBanner ? (
							<FloatingChatFailedBanner />
						) : (
							<AgentPickerContext.Provider value={pickerState}>
								<AssistantRuntimeProvider
									key={conversationId ?? "new"}
									runtime={runtime}
								>
									<Thread
										components={THREAD_COMPONENTS}
										environmentReady={environmentReady}
										// A chat_message trigger always persists the user's own
										// message before the agent runs (see handler.Handle), so
										// a failed/recoverable turn almost never has
										// messages.length === 0 — showFailedBanner above
										// essentially never fires for the common case. Rendered
										// inside the message flow (not a page footer below the
										// composer, which read as small print easy to miss) so a
										// failure with a visible message still explains itself.
										viewportOverlay={
											conversation?.error_message ? (
												<ConversationErrorBox />
											) : undefined
										}
									/>
								</AssistantRuntimeProvider>
							</AgentPickerContext.Provider>
						)}
					</div>
				</div>
			)}
		</>
	);
}
