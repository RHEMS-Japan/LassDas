import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";

const html=readFileSync(process.argv[2],"utf8");
const start=html.indexOf("function applyPayload("),end=html.indexOf("</script>",start);
assert.ok(start>=0 && end>start);
let now=Date.parse("2026-09-20T12:00:00Z"),timer,stream,rendered;
const nodes=new Map();
const node=id=>{
 if(!nodes.has(id)) nodes.set(id,{textContent:"",classList:{toggle(){}}});
 return nodes.get(id);
};
class Clock extends Date { static now(){return now;} }
class Events {
 static CLOSED=2;
 constructor(){stream=this;this.handlers={};}
 addEventListener(name,handler){this.handlers[name]=handler;}
}
const context=vm.createContext({
 Date:Clock,document:{getElementById:node,querySelectorAll:()=>[]},EventSource:Events,
 setTimeout(){},setInterval(fn){timer=fn;},
 adoptStages(){},renderEvents(){},actKey(){},fmtElapsed(){},
 renderBoard(){rendered=vm.runInContext("latestBoard",context);},
});
vm.runInContext("let lastPayloadAt=0,lastServerSentAt=null,trackerBase='',actionsEnabled=false,acknowledgeEnabled=false,sentActions=new Map(),latestBoard={runs:[]},snapshotState='missing',snapshotAgeAtReceive=null,feedConnected=false;\n"+html.slice(start,end),context);
function send(state,board,sent=now) {
 stream.handlers.board({data:JSON.stringify({snapshot_state:state,board,sent_at:new Date(sent).toISOString(),actions:[],events:[]})});
 timer();
}
function fresh(step="implement") {return {schema_version:1,generated_at:new Date(now).toISOString(),runs:[{state:"claimed",step}]};}
send("missing",{schema_version:1,runs:[]});
assert.equal(node("live-text").textContent,"進捗未確認");
assert.match(node("conn-banner").textContent,/未確認/);
assert.match(node("counts").textContent,/未確認/);
console.log("PASS absent snapshot is not ONLINE");
send("ready",fresh());
assert.equal(node("live-text").textContent,"ONLINE");assert.equal(rendered.runs[0].step,"implement");
console.log("PASS fresh snapshot is rendered");
send("ready",fresh("review"));
assert.equal(rendered.runs[0].step,"review");
console.log("PASS next event advances the displayed stage without reload");
send("invalid",{schema_version:1,runs:[]});
assert.equal(node("live-text").textContent,"進捗未確認");assert.equal(rendered.runs[0].step,"review");
console.log("PASS corrupt snapshot retains last good data with warning");
send("ready",{schema_version:1,generated_at:"2020-01-01T00:00:00Z",runs:[]});
assert.equal(node("live-text").textContent,"進捗未確認");
console.log("PASS fresh delivery time cannot disguise old progress");
send("ready",fresh());
now+=181000;timer();assert.notEqual(node("live-text").textContent,"ONLINE");
assert.ok(vm.runInContext("snapshotProblem()",context));
console.log("PASS progress expires without a new event");
send("ready",fresh("checks"));
assert.equal(node("live-text").textContent,"ONLINE");
console.log("PASS a fresh snapshot recovers the warning");
stream.handlers.board({data:"{"});
assert.equal(node("live-text").textContent,"進捗未確認");
console.log("PASS malformed event is not treated as healthy");
const server=now-3600000;
send("ready",{schema_version:1,generated_at:new Date(server).toISOString(),runs:[]},server);
assert.equal(node("live-text").textContent,"ONLINE");
console.log("PASS browser clock skew does not make fresh data stale");
stream.onerror();assert.equal(node("live-text").textContent,"再接続中…");
console.log("PASS transport loss is distinct from snapshot failure");
