import { expect, test } from "bun:test";
import Redis from "ioredis";
import pino from "pino";
import { createSocketServer } from "./server.ts";
import { DeliveryGate, createSubscriber, routeEvent } from "./subscriber.ts";
import { fetchCurrentRecipients, internalRequest } from "./api-client.ts";

// Invoked by the Go E2E fixture with real PostgreSQL/API policy and Valkey.
// Raw Engine.IO/WebSocket packets exercise the installed Socket.IO server,
// including a malicious recovery-shaped handshake, without a new dependency.
test.skipIf(!process.env.PACA_DELIVERY_FIXTURE)("US014Revocation", async () => {
 const base=process.env.PACA_DELIVERY_FIXTURE!;
 const fixture=await (await fetch(`${base}/fixture`)).json() as any;
 const redis=new Redis(fixture.redis); const controls=new Redis(fixture.redis);
 const logger=pino({level:"silent"});
 const {io,httpServer}=createSocketServer({port:0,apiUrl:base,internalAPIKey:fixture.key,instanceID:fixture.instance,valkey:{url:fixture.redis},cors:{origins:["*"]},logLevel:"silent"},redis,logger);
 await new Promise<void>(resolve=>httpServer.listen(0,"0.0.0.0",resolve));
 const emitted:any[]=[];const adapter=io.of("/").adapter;const broadcast=adapter.broadcast.bind(adapter);adapter.broadcast=(packet,options)=>{emitted.push(packet.data?.[1]);broadcast(packet,options)};
 const address=httpServer.address() as {port:number};
 const gate=new DeliveryGate(()=>io.emit("authorization.changed"));const boot=crypto.randomUUID();
 const channel=`paca:${fixture.instance}:authorization:control`,acks=`paca:${fixture.instance}:authorization:acks`;
 let opened: (()=>void)|undefined;
 let openHold: Promise<void>|undefined;
 controls.on("message",(_channel,raw)=>{void(async()=>{
  const m=JSON.parse(raw);
  if(m.type==="close") {gate.update(m.epoch,true);await redis.publish(acks,JSON.stringify({type:"closed",epoch:m.epoch,boot_id:boot}));}
  if(m.type==="reconcile")await redis.publish(acks,JSON.stringify({type:"reconciled",epoch:m.epoch,boot_id:boot}));
  if(m.type==="open"){const s=await internalRequest(base,fixture.key,"authorization-delivery/state",undefined,"GET");if(s.epoch===m.epoch&&!s.blocked){await openHold;gate.update(m.epoch,false);opened?.();}}
 })();});
 await controls.subscribe(channel);
 const state=await internalRequest(base,fixture.key,"authorization-delivery/participants",{boot_id:boot,kind:"realtime"});gate.update(state.epoch,state.blocked);
 const eventSubscriber=createSubscriber(fixture.redis,io,logger,gate,scope=>fetchCurrentRecipients(base,fixture.key,scope));
 await eventSubscriber.subscribe("paca.events");
 const sockets: WebSocket[]=[];
 async function connect(token:string,recovery?:{pid:string,offset:string}) {
  const ws=new WebSocket(`ws://127.0.0.1:${address.port}/socket.io/?EIO=4&transport=websocket`);sockets.push(ws);
  const events:any[]=[];const signals:any[]=[];const waiters:((v:any)=>void)[]=[];let sid="",pid="",offset="";
  let ready!:()=>void;const connected=new Promise<void>(r=>{ready=r});
  ws.onmessage=({data})=>{const raw=String(data);if(raw.startsWith("0"))ws.send("40"+JSON.stringify({token,...recovery}));else if(raw==="2")ws.send("3");else if(raw.startsWith("40")){const auth=JSON.parse(raw.slice(2));sid=auth.sid;pid=auth.pid??sid;ready();}else if(raw.startsWith("42")){const packet=JSON.parse(raw.slice(2));if(packet[0]==="authorization.changed"){signals.push(packet.slice(1));return;}offset=packet[2]??"attempted-offset";events.push(packet[1]);waiters.shift()?.(packet[1]);}};
  await Promise.race([connected,Bun.sleep(3000).then(()=>{throw Error("Socket.IO handshake timeout")})]);
  return {ws,events,signals,get pid(){return pid},get offset(){return offset},next:()=>new Promise<any>(r=>waiters.push(r))};
 }
 try {
  const a=await connect(fixture.tokenA),b=await connect(fixture.tokenB);
  const event=(marker:string)=>({type:"task.updated",payload:{project_id:fixture.project,task_id:fixture.task,activity_type:"task.updated",content:{changes:[]},marker}});
  const recipients=(scope:Record<string,unknown>)=>fetchCurrentRecipients(base,fixture.key,scope);
  const first=a.next();await routeEvent(io,event("lawful-before-close"),logger,gate,recipients);expect((await first).payload.marker).toBe("lawful-before-close");expect(a.events.length).toBe(1);expect(b.events.length).toBe(0);
  await fetch(`${base}/hold`,{method:"POST"});
  const held=routeEvent(io,event("HELD_SECRET"),logger,gate,recipients);
  await fetch(`${base}/computed`);
  let releaseOpen!:()=>void;openHold=new Promise<void>(r=>{releaseOpen=r});
  const reopen=new Promise<void>(r=>{opened=r});
  const revoked=await fetch(`${base}/revoke`,{method:"POST"});expect(revoked.status).toBe(200);
  expect(gate.blocked).toBe(true);
  const buffered=b.next();
  const consumed=new Promise<void>(resolve=>{const listener=(channel:string,raw:string)=>{if(channel==="paca.events"&&JSON.parse(raw).payload?.marker==="original-during-open"){eventSubscriber.off("message",listener);resolve();}};eventSubscriber.on("message",listener)});
  await fetch(`${base}/retry-recipients`,{method:"POST"});
  await redis.publish("paca.events",JSON.stringify(event("original-during-open")));await consumed;
  expect(emitted.some(e=>e?.payload?.marker==="original-during-open")).toBe(false);
  openHold=undefined;releaseOpen();await reopen;
  expect((await Promise.race([buffered,Bun.sleep(1000).then(()=>({payload:{marker:"timeout"}}))])).payload.marker).toBe("original-during-open");
  expect(b.events.filter(e=>e.payload?.marker==="original-during-open").length).toBe(1);
  expect(a.events.filter(e=>e.payload?.marker==="original-during-open").length).toBe(0);
  const truth=await (await fetch(`${base}/truth`)).json() as any;expect(truth.assignees).toEqual([fixture.memberB]);
  await fetch(`${base}/release`,{method:"POST"});await held;expect(emitted.some(e=>e?.payload?.marker==="HELD_SECRET")).toBe(false);
  gate.update(state.epoch,false); // an enrollment snapshot delayed across CLOSE
  expect(gate.epoch).not.toBe(state.epoch);
  const fresh=b.next();await routeEvent(io,event("fresh-B"),logger,gate,recipients);expect((await fresh).payload.marker).toBe("fresh-B");
  expect(b.events.length).toBe(2);expect(a.events.length).toBe(1);
  const restoreOpen=new Promise<void>(r=>{opened=r});
  expect((await fetch(`${base}/restore`,{method:"POST"})).status).toBe(200);await restoreOpen;
  const recovery={pid:a.pid,offset:a.offset};
  await new Promise<void>(resolve=>{a.ws.onclose=()=>resolve();a.ws.close();});
  // A legally addressed offline packet must not be cached for replay after revoke.
  await routeEvent(io,event("OFFLINE_SECRET"),logger,gate,recipients);
  const againOpen=new Promise<void>(r=>{opened=r});
  expect((await fetch(`${base}/revoke`,{method:"POST"})).status).toBe(200);await againOpen;
  const reconnected=await connect(fixture.tokenA,recovery);
  expect(reconnected.events).toEqual([]);
  const history=await fetch(`${base}/history`,{headers:{Authorization:`Bearer ${fixture.tokenA}`}});expect(history.status).toBe(200);const saved=await history.text();expect(saved).toContain(fixture.notification);expect(saved).not.toContain("FRESH_HIDDEN_TITLE");
  const auth=await (await fetch(`${base}/auth-count`)).json() as any;expect(auth.count).toBe(3);
  async function until(check:()=>boolean) {
   const end=Date.now()+5000;
   while(!check()){if(Date.now()>end)throw Error("document Socket delivery timeout");await Bun.sleep(10);}
  }
  const request=async(token:string,method:string,path:string,body?:unknown)=>{
   const response=await fetch(`${fixture.api}/api/v1/projects/${fixture.project}/docs${path}`,{method,headers:{Authorization:`Bearer ${token}`,"Content-Type":"application/json"},body:body===undefined?undefined:JSON.stringify(body)});
   const raw=await response.text();if(response.status!==200&&response.status!==201&&response.status!==204)throw Error(`${method} ${path}: ${response.status} ${raw}`);
   return raw?JSON.parse(raw).data:null;
  };
  const eventCount=(socket:any,type:string,id:string)=>socket.events.filter((e:any)=>e.type===type&&e.payload?.document_id===id).length;
  const cases:any[]=[];
  const privateDoc=await request(fixture.tokenA,"POST","",{title:"SOCKET_PRIVATE_A",content:[]});cases.push([privateDoc,reconnected,b,fixture.tokenA]);
  const taskDoc=await request(fixture.managerToken,"POST","",{title:"SOCKET_TASK_B",visibility:"task",task_ids:[fixture.task],content:[]});cases.push([taskDoc,b,reconnected,fixture.tokenB]);
  const source=await request(fixture.managerToken,"POST","",{title:"SOCKET_CANONICAL",visibility:"project",content:[]});
  const reference=await request(fixture.tokenA,"POST","",{title:"SOCKET_REFERENCE_A",source_document_id:source.id});cases.push([reference,reconnected,b,fixture.tokenA]);
  for(const [doc,allowed,denied,token] of cases){
   await until(()=>eventCount(allowed,"doc.created",doc.id)>0);
   expect(eventCount(denied,"doc.created",doc.id)).toBe(0);
   await request(token,"PATCH",`/${doc.id}`,{title:doc.title+"_UPDATED"});
   await until(()=>eventCount(allowed,"doc.updated",doc.id)>0);
   expect(eventCount(denied,"doc.updated",doc.id)).toBe(0);
   const comment=await request(token,"POST",`/${doc.id}/comments`,{content:[{type:"paragraph",content:[{type:"text",text:"SOCKET_COMMENT"}]}]});
   await until(()=>eventCount(allowed,"doc.comment.added",doc.id)>0);
   await request(token,"PATCH",`/${doc.id}/comments/${comment.id}`,{content:[{type:"paragraph",content:[{type:"text",text:"SOCKET_EDITED"}]}]});
   await until(()=>eventCount(allowed,"doc.comment.updated",doc.id)>0);
   await request(token,"DELETE",`/${doc.id}/comments/${comment.id}`);
   await until(()=>eventCount(allowed,"doc.comment.deleted",doc.id)>0);
   for(const type of ["doc.comment.added","doc.comment.updated","doc.comment.deleted"])expect(eventCount(denied,type,doc.id)).toBe(0);
  }
  await request(fixture.managerToken,"PATCH",`/${source.id}/access`,{visibility:"private",expected_access_version:1});
  await until(()=>!gate.blocked);
  const before=reconnected.events.length;
  await routeEvent(io,{type:"doc.updated",payload:{project_id:fixture.project,document_id:reference.id,content:"REVOKED_CANONICAL"}},logger,gate,recipients);
  await routeEvent(io,{type:"doc.updated",payload:{project_id:fixture.project,content:"MISSING_ID"}},logger,gate,recipients);
  await routeEvent(io,{type:"doc.updated",payload:{project_id:crypto.randomUUID(),document_id:privateDoc.id,content:"WRONG_PROJECT"}},logger,gate,recipients);
  expect(reconnected.events.length).toBe(before);
  const deniedRead=await fetch(`${fixture.api}/api/v1/projects/${fixture.project}/docs/${reference.id}`,{headers:{Authorization:`Bearer ${fixture.tokenA}`}});expect(deniedRead.status).toBe(404);
  const restored=new Promise<void>(r=>{opened=r});await fetch(`${base}/restore`,{method:"POST"});await restored;
  const taskBefore=eventCount(b,"doc.updated",taskDoc.id);
  await request(fixture.tokenA,"PATCH",`/${taskDoc.id}`,{title:"NOW_TASK_A"});
  await until(()=>eventCount(reconnected,"doc.updated",taskDoc.id)>0);
  expect(eventCount(b,"doc.updated",taskDoc.id)).toBe(taskBefore);
  await request(fixture.tokenA,"DELETE",`/${privateDoc.id}`);
  await until(()=>!gate.blocked);
  expect(eventCount(reconnected,"doc.deleted",privateDoc.id)).toBe(0);
  expect(reconnected.signals.length).toBeGreaterThan(0);
  expect(reconnected.signals.every((s:any)=>s.length===0)).toBe(true);
  const afterSourceRevoke=await connect(fixture.tokenA);
  await routeEvent(io,{type:"doc.updated",payload:{project_id:fixture.project,document_id:reference.id,content:"OFFLINE_SOURCE_SECRET"}},logger,gate,recipients);
  expect(afterSourceRevoke.events).toEqual([]);

 } finally {
  for(const s of sockets)s.close();
  await new Promise<void>(r=>io.close(()=>r()));
  await internalRequest(base,fixture.key,`authorization-delivery/participants/${boot}`,undefined,"DELETE");
  eventSubscriber.disconnect();controls.disconnect();redis.disconnect();
 }
},60000);
