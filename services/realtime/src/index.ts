// Realtime service entry point.
//
// Starts the Socket.IO server and the Valkey Pub/Sub subscriber, then blocks
// until a SIGTERM or SIGINT signal triggers a graceful shutdown.
//
// Two separate ioredis clients are created:
//   sessionRedis  — regular R/W client for session data (join/leave/disconnect).
//   subscriber    — client locked into subscriber mode (subscribe/psubscribe only).
//                   ioredis requires a dedicated client for this mode.

import Redis from "ioredis";
import pino from "pino";
import { loadConfig } from "./config.ts";
import { createSocketServer } from "./server.ts";
import { fetchCurrentRecipients, internalRequest } from "./api-client.ts";
import { DeliveryGate, createSubscriber } from "./subscriber.ts";

const config = loadConfig();

const logger = pino({
	level: config.logLevel,
	...(process.env.NODE_ENV !== "production" && {
		transport: { target: "pino-pretty", options: { colorize: true } },
	}),
});

logger.info("starting paca realtime service");

// Session store client — regular command mode.
const sessionRedis = new Redis(config.valkey.url, {
	lazyConnect: false,
	maxRetriesPerRequest: null,
});

sessionRedis.on("connect", () =>
	logger.info("valkey session client connected"),
);
sessionRedis.on("error", (err: unknown) =>
	logger.error({ err }, "valkey session client error"),
);

const { httpServer, io } = createSocketServer(config, sessionRedis, logger);
const gate = new DeliveryGate();
const subscriber = createSubscriber(config.valkey.url, io, logger, gate,
 scope=>fetchCurrentRecipients(config.apiUrl,config.internalAPIKey,scope));
const bootID=crypto.randomUUID();
const controlChannel=`paca:${config.instanceID}:authorization:control`;
const ackChannel=`paca:${config.instanceID}:authorization:acks`;
let enrolled=false;
let connectionGeneration=0;
const syncState=async()=>{
 const generation=connectionGeneration;
 gate.disconnect();
 await subscriber.subscribe(controlChannel);
 const state=await internalRequest(config.apiUrl,config.internalAPIKey,
  enrolled?"authorization-delivery/state":"authorization-delivery/participants",
  enrolled?undefined:{boot_id:bootID,kind:"realtime"},enrolled?"GET":"POST");
 enrolled=true;
 if(typeof state.epoch!=="string"||typeof state.blocked!=="boolean")throw Error("invalid delivery state");
 if(generation===connectionGeneration&&subscriber.status==="ready")gate.update(state.epoch,state.blocked);
};
subscriber.on("close",()=>{connectionGeneration++;gate.disconnect();});
subscriber.on("error",()=>{connectionGeneration++;gate.disconnect();});
subscriber.on("ready",()=>{void syncState().catch(()=>logger.error("delivery enrollment unavailable"));});
subscriber.on("message",(channel:string,raw:string)=>{
 if(channel!==controlChannel)return;
 void (async()=>{
  const msg=JSON.parse(raw);
  if(typeof msg.epoch!=="string")return;
  if(msg.type==="close"){
   gate.update(msg.epoch,true);
   if(gate.epoch===msg.epoch)await sessionRedis.publish(ackChannel,JSON.stringify({type:"closed",epoch:msg.epoch,boot_id:bootID}));
  }else if(msg.type==="reconcile"&&gate.blocked&&gate.epoch===msg.epoch){
   await sessionRedis.publish(ackChannel,JSON.stringify({type:"reconciled",epoch:msg.epoch,boot_id:bootID}));
  }else if(msg.type==="open"&&enrolled&&subscriber.status==="ready"){
   const generation=connectionGeneration;
   const state=await internalRequest(config.apiUrl,config.internalAPIKey,"authorization-delivery/state",undefined,"GET");
   if(generation===connectionGeneration&&subscriber.status==="ready"&&state.epoch===msg.epoch&&!state.blocked)gate.update(msg.epoch,false);
  }
 })().catch(()=>{gate.disconnect();logger.error("delivery control unavailable");});
});

httpServer.listen(config.port, () => {
	logger.info({ port: config.port }, "realtime service listening");
});

// ── Graceful shutdown ────────────────────────────────────────────────────────

function shutdown(signal: string): void {
 gate.disconnect();
	logger.info({ signal }, "shutdown signal received");

	// Close Socket.IO first to actively terminate WebSocket connections,
	// then close the HTTP server so it stops accepting new connections.
	io.close(() => {
		httpServer.close(async () => {
			await internalRequest(config.apiUrl,config.internalAPIKey,`authorization-delivery/participants/${bootID}`,undefined,"DELETE").catch(()=>logger.error("delivery unenrollment unavailable"));
 subscriber.disconnect();
			sessionRedis.disconnect();
			logger.info("shutdown complete");
			process.exit(0);
		});
	});

	setTimeout(() => {
		logger.warn("forced shutdown after timeout");
		process.exit(1);
	}, 10_000).unref();
}

process.on("SIGTERM", () => shutdown("SIGTERM"));
process.on("SIGINT", () => shutdown("SIGINT"));
