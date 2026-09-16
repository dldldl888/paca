import Redis from "ioredis";
import type { Logger } from "pino";
import type { Server } from "socket.io";
import { agentChatRoomName } from "./permissions.ts";
import { ApiRequestError } from "./api-client.ts";

// CHANNEL is the Valkey Pub/Sub channel the API publishes to.
// Must stay in sync with events.ChannelRealtime in services/api.
const CHANNEL = "paca.events";

interface RealtimeMessage {
	type: string;
	payload?: unknown;
	data?: unknown;
}

// createSubscriber connects a dedicated ioredis client in subscriber mode,
// subscribes to CHANNEL, and wires incoming messages to Socket.IO rooms.
// Returns the ioredis client so the caller can disconnect it on shutdown.
export function createSubscriber(
	valkeyUrl: string,
	io: Server,
	logger: Logger,
 gate: DeliveryGate,
 recipients: (scope: Record<string, unknown>) => Promise<Recipients>,
): Redis {
	// A separate client is required because ioredis clients in subscriber mode
	// cannot issue regular commands.
	const client = new Redis(valkeyUrl, {
		autoResubscribe: true,
		lazyConnect: false,
		maxRetriesPerRequest: null,
	});

	client.on("connect", () => {
		logger.info("valkey subscriber connected");
	});

	client.on("reconnecting", () => {
		logger.warn("valkey subscriber reconnecting");
	});

	client.on("error", (err: unknown) => {
		logger.error({ err }, "valkey subscriber error");
	});

	client.subscribe(CHANNEL, (err, count) => {
		if (err) {
			logger.error({ err }, `failed to subscribe to ${CHANNEL}`);
		} else {
			logger.info(
				{ channel: CHANNEL, subscriptions: count },
				"subscribed to valkey channel",
			);
		}
	});

	client.on("message", (_channel: string, raw: string) => {
 if(_channel!==CHANNEL)return;
		let msg: RealtimeMessage;
		try {
			msg = JSON.parse(raw) as RealtimeMessage;
		} catch (err) {
			logger.warn({ err }, "failed to parse valkey message — skipping");
			return;
		}

		void routeEvent(io, msg, logger, gate, recipients);
	});

	return client;
}

export class DeliveryGate {
 epoch = "0";
 blocked = true;
 private pending = new Set<() => void>();
 afterOpen(action: () => void): void { this.pending.add(action); }
 update(epoch: string, blocked: boolean): void {
  if (!/^(0|[1-9][0-9]*)$/.test(epoch) || BigInt(epoch) < BigInt(this.epoch)) return;
  this.epoch = epoch;
  this.blocked = blocked;
  if (!blocked) { const pending = [...this.pending]; this.pending.clear(); for (const action of pending) action(); }
 }
 disconnect(): void { this.blocked = true; }
}

export interface Recipients { epoch: string; user_ids: string[] }
export async function routeEvent(io: Server, msg: RealtimeMessage, logger: Logger,
 gate: DeliveryGate, recipients: (scope: Record<string, unknown>) => Promise<Recipients>): Promise<void> {
 const { type } = msg;
 let payload = eventPayload(msg);
 if (!payload || typeof type !== "string") return;
 if(type==="agent.authorization_revoked") {
  payload={conversation_id:payload.conversation_id,status:"failed"};
 }
 // OPEN is verified asynchronously. Keep the original event until that check
 // completes, then resolve current recipients rather than retaining an allow.
 const deferUntilOpen = () => gate.afterOpen(() => {
  void routeEvent(io, {type, payload}, logger, gate, recipients);
 });
 if (gate.blocked) { deferUntilOpen(); return; }
 const scope: Record<string, unknown> = {event_type:type};
 for (const key of ["project_id", "task_id", "conversation_id", "recipient_user_id", "activity_id", "activity_type", "content"]) {
  if (payload[key] !== undefined) scope[key] = payload[key];
 }
 // Activity publication can precede persistence; typed metadata is checked by the API.
 if (type === "task.activity.created") {
  scope.activity_id = payload.id ?? payload.activity_id;
  scope.activity_type = payload.type ?? payload.activity_type;
 }
 for (;;) {
  if (gate.blocked) { deferUntilOpen(); return; }
  try {
  const allow = await recipients(scope);
  if (gate.blocked) { deferUntilOpen(); return; }
  if (allow.epoch !== gate.epoch || !Array.isArray(allow.user_ids)) return;
  const users = [...new Set(allow.user_ids)];
  if (!users.length || users.some(id => typeof id !== "string" || !id)) return;
  const notification = type.startsWith("notification.");
  const rooms = users.map(id => notification ? `user:${id}:notifications` : agentChatRoomName(id));
  // The installed synchronous adapter hands off in this JS turn: no await here.
  io.to(rooms).emit(notification ? "notification" : "event", {type,payload});
  return;
 } catch (err) {
  // The API participant can observe OPEN after realtime. Retry this event;
  // ordinary authorization/dependency failures retain their fail-closed path.
  if (err instanceof ApiRequestError && err.status === 409) {
   await new Promise<void>(resolve => setTimeout(resolve, 25));
   continue;
  }
  logger.warn({type}, "current event authorization unavailable");
  return;
 }
 }
}

function eventPayload(msg: RealtimeMessage): Record<string, unknown> | null {
	if (msg.payload && typeof msg.payload === "object") {
		return msg.payload as Record<string, unknown>;
	}
	if (msg.data && typeof msg.data === "object") {
		return msg.data as Record<string, unknown>;
	}
	return null;
}
