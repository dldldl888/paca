import { expect, test } from "bun:test";
import type { Server } from "socket.io";
import { DeliveryGate, routeEvent } from "./subscriber.ts";
import { ApiRequestError } from "./api-client.ts";

const logger = { debug() {}, warn() {}, error() {} } as any;
function recorder() {
 const emitted: any[] = [];
 const io = { to(rooms: unknown) { return { emit(channel: string, event: unknown) { emitted.push({rooms, channel, event}); } }; } } as unknown as Server;
 return {io, emitted};
}
test("US007RecipientRouting", async () => {
 const {io, emitted} = recorder();
 const gate = new DeliveryGate();
 gate.update("9007199254740993", false);
 let release!: (v: any) => void;
 const held = new Promise<any>(resolve => { release = resolve; });
 const event = {type:"task.updated",payload:{project_id:"p",task_id:"t",title:"private marker"}};
 const pending = routeEvent(io,event,logger,gate,async () => held);
 gate.update("9007199254740994",true);
 gate.update("9007199254740994",false);
 gate.update("9007199254740993",false); // delayed enrollment/OPEN cannot roll back
 release({epoch:"9007199254740993",user_ids:["A"]});
 await pending;
 expect(emitted).toEqual([]);
 await routeEvent(io,event,logger,gate,async () => ({epoch:"9007199254740994",user_ids:["B","B"]}));
 expect(emitted).toEqual([{rooms:["user:B:agent-chat"],channel:"event",event}]);
});
test("US007FailClosed", async () => {
 const {io,emitted} = recorder(); const gate = new DeliveryGate(); gate.update("1",false);
 const event = {type:"agent.agent_message_chunk",payload:{conversation_id:"c",project_id:"p",actor_user_id:"forged",text:"secret"}};
 await routeEvent(io,event,logger,gate,async () => ({epoch:"1",user_ids:["A"]}));
 expect(emitted.length).toBe(1);
 gate.update("2",true);
 await routeEvent(io,event,logger,gate,async () => ({epoch:"2",user_ids:[]}));
 gate.update("2",false);
 await routeEvent(io,event,logger,gate,async () => {throw Error("unavailable");});
 await routeEvent(io,{type:"task.updated",payload:{project_id:"p"}},logger,gate,async () => {throw Error("missing task");});
 expect(emitted.length).toBe(1);
});
test("new process and disconnected participant start closed", async () => {
 const {io,emitted} = recorder(); const gate = new DeliveryGate();
 await routeEvent(io,{type:"task.updated",payload:{task_id:"t",project_id:"p"}},logger,gate,async () => ({epoch:"0",user_ids:["A"]}));
 expect(emitted).toEqual([]);
});

test("content-free revoked terminal waits for OPEN and current owner",async()=>{
 const gate=new DeliveryGate();let resolve!:()=>void;const delivered=new Promise<void>(r=>{resolve=r});const events:any[]=[];
 const io={to(rooms:unknown){return{emit(_channel:string,event:unknown){events.push({rooms,event});resolve()}}}} as unknown as Server;
 await routeEvent(io,{type:"agent.authorization_revoked",payload:{conversation_id:"private-id",text:"SECRET",project_id:"SECRET"}},logger,gate,async scope=>{expect(scope).toEqual({event_type:"agent.authorization_revoked",conversation_id:"private-id"});return{epoch:"1",user_ids:["owner"]}});
 expect(events).toEqual([]);gate.update("1",false);await delivered;
 expect(events).toEqual([{rooms:["user:owner:agent-chat"],event:{type:"agent.authorization_revoked",payload:{conversation_id:"private-id",status:"failed"}}}]);
});


test("original event waits for OPEN and API participant before current delivery", async () => {
 const {io,emitted}=recorder();const gate=new DeliveryGate();gate.update("2",true);
 let calls=0;let completed!:()=>void;const delivered=new Promise<void>(r=>{completed=r});
 const target={to(rooms:unknown){return{emit(channel:string,event:unknown){(io.to(rooms as string[]) as any).emit(channel,event);completed()}}}} as unknown as Server;
 const event={type:"task.updated",payload:{project_id:"p",task_id:"t",activity_type:"task.updated",content:{changes:[]},marker:"original"}};
 await routeEvent(target,event,logger,gate,async()=>{calls++;if(calls<=2)throw new ApiRequestError("transition pending",409);return{epoch:"2",user_ids:["B","B"]}});
 expect(calls).toBe(0);expect(emitted).toEqual([]);
 gate.update("2",false);
 await Promise.race([delivered,Bun.sleep(1000)]);
 expect(calls).toBe(3);
 expect(emitted).toEqual([{rooms:["user:B:agent-chat"],channel:"event",event}]);
});

test("doc identity reaches current checker and folders only refresh classification", async()=>{
 const {io,emitted}=recorder();const gate=new DeliveryGate();gate.update("1",false);
 let received:any;
 const recipients=async(scope:any)=>{received=scope;return {epoch:"1",user_ids:["A"]}};
 await routeEvent(io,{type:"doc.comment.updated",payload:{project_id:"p",document_id:"d",activity_id:"a",activity_type:"doc.comment.added",content:'{"text":"comment"}'}},logger,gate,recipients);
 expect(received.document_id).toBe("d");
 await routeEvent(io,{type:"doc.folder.updated",payload:{project_id:"p",id:"folder",name:"name",content:"untrusted body"}},logger,gate,recipients);
 expect(emitted[1].event.payload).toEqual({project_id:"p"});
});

test("verified OPEN emits a content-free reauthorization signal without rolling back epochs",()=>{
 const signals:unknown[]=[];
 const gate=new DeliveryGate(()=>signals.push({}));
 gate.update("3",false);gate.update("2",false);gate.update("4",true);
 expect(signals).toEqual([{}]);
 gate.update("4",false);gate.update("4",false);
 expect(signals).toEqual([{},{}]);
 gate.disconnect();gate.update("4",false);
 expect(signals).toEqual([{},{},{}]);
});
