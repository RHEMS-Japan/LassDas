// The two lanes, measured against the page's own code.
//
// A finished card used to drop into 完了・終了 by itself, the moment the
// engine called the run finished, and it said only what it had ended as.
// Nobody could tell a run that ended minutes ago from one that ended the
// day before, and nobody had looked at either. The rules below are what a
// reader now experiences: a finished card waits in the running lane with
// its time on it until a person clears it away, and the counts line is
// built from the very same split.
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";

const html = readFileSync(process.argv[2], "utf8");
const script = [...html.matchAll(/<script>([\s\S]*?)<\/script>/g)].map(m => m[1]).join("\n");
// The lane code, from the first of its declarations to the one after it.
// Every name driven below has to be inside the slice: a declaration moved
// out of it would be silently untested, which is how a harness stops
// testing the thing it names.
const from = script.indexOf("const FINISHED");
const to = script.indexOf("const el = (tag");
const body = from < 0 || to < 0 || to < from ? "" : script.slice(from, to);
const required = ["isFinished", "acknowledgedAt", "isCleared", "fmtStamp", "splitLanes", "countsOf", "localAcknowledged"];
const missing = required.filter(name => !body.includes(name));
if (!body || missing.length) {
  console.log("FAIL harness: the lane code is not all in one place; missing " + JSON.stringify(missing));
  process.exit(1);
}
const context = vm.createContext({ ACTIONABLE: new Set(["confirm", "question"]) });
vm.runInContext(body, context);
const call = (name, ...args) => vm.runInContext(name, context)(...args);

const finished = (id, step, extra = {}) => ({ delivery_id: id, step, step_title: step, claimed_at_ms: 1000, ...extra });
const running = (id, step, claimed) => ({ delivery_id: id, step, claimed_at_ms: claimed });

// 1. A finished run nobody has cleared away stays where a person will see it.
for (const step of ["done", "stopped", "failed"]) {
  const lanes = call("splitLanes", [finished("d-" + step, step, { finished_at: "2026-09-25T02:00:00Z" })]);
  assert.equal(lanes.cleared.length, 0, step + " archived itself");
  assert.equal(lanes.active.length, 1);
  assert.equal(lanes.awaiting.length, 1);
  assert.equal(lanes.running.length, 0);
}
console.log("PASS every finished state waits in the running lane");

// 2. Acknowledging it moves it, and nothing else does.
const acked = finished("d1", "done", { finished_at: "2026-09-25T02:00:00Z", acknowledged_at: "2026-09-25T04:30:00Z" });
let lanes = call("splitLanes", [acked]);
assert.equal(lanes.cleared.length, 1);
assert.equal(lanes.active.length, 0);
console.log("PASS an acknowledged card is in the finished lane");

// 3. The acknowledgement this browser just made moves the card before the
// next payload carries it back.
const pressed = finished("d2", "failed", { finished_at: "2026-09-25T02:00:00Z" });
assert.equal(call("isCleared", pressed), false);
vm.runInContext("localAcknowledged", context).set("d2", "2026-09-25T05:00:00Z");
assert.equal(call("isCleared", pressed), true);
assert.equal(call("acknowledgedAt", pressed), "2026-09-25T05:00:00Z");
vm.runInContext("localAcknowledged", context).delete("d2");
console.log("PASS the card moves as soon as the press is written down");

// 4. A running card is never in the finished lane, acknowledged or not.
const stillGoing = running("d3", "implement", 5000);
stillGoing.acknowledged_at = "2026-09-25T05:00:00Z";
lanes = call("splitLanes", [stillGoing]);
assert.equal(lanes.cleared.length, 0);
assert.equal(lanes.running.length, 1);
console.log("PASS a running card cannot be archived");

// 5. The counts line is the same split the cards came from.
const rows = [
  finished("a", "done", { finished_at: "2026-09-25T02:00:00Z" }),
  finished("b", "failed", { finished_at: "2026-09-25T02:00:00Z", acknowledged_at: "2026-09-25T03:00:00Z" }),
  running("c", "confirm", 3000),
  running("d", "implement", 4000),
  running("e", "attention", 2000),
];
lanes = call("splitLanes", rows);
const tally = call("countsOf", lanes);
// Field by field: the tally is built inside the page's own context, so it
// is not the same kind of object a deep comparison here would demand.
assert.deepEqual({ ...tally }, { running: 3, attention: 2, awaiting: 1, cleared: 1 });
assert.equal(tally.running + tally.awaiting, lanes.active.length);
assert.equal(tally.cleared, lanes.cleared.length);
assert.equal(tally.running + tally.awaiting + tally.cleared, rows.length);
console.log("PASS the counts line adds up to the cards on the board");

// 6. Whoever has to do something is at the top: a decision first, then a
// card waiting to be cleared, then the ones still running.
assert.deepEqual([...lanes.active].map(run => run.delivery_id), ["c", "a", "d", "e"]);
console.log("PASS the lane is ordered by who has to act");

// 7. Both times read on one clock, whatever the browser's own zone is.
process.env.TZ = "America/New_York";
const stamp = call("fmtStamp", "2026-09-25T02:00:00Z");
assert.match(stamp, /2026\/09\/25 11:00 JST$/, "stamp = " + stamp);
assert.equal(call("fmtStamp", ""), "");
assert.equal(call("fmtStamp", "not a time"), "");
console.log("PASS a time is shown on the engine's clock");

// 8-11. What a finished card actually says. The times are the whole reason
// the card waits here, so they are read off the card the page builds, not
// inferred from the row.
const guidanceFrom = script.indexOf("function buildGuidance(run) {");
const guidanceTo = script.indexOf("function buildResolveRow(run) {");
const ackFrom = script.indexOf("function acknowledgeState(deliveryID) {");
const ackTo = script.indexOf("function decisionState(");
if (guidanceFrom < 0 || guidanceTo < guidanceFrom || ackFrom < 0 || ackTo < ackFrom) {
  console.log("FAIL harness: the card's guidance block is not where this check looks for it");
  process.exit(1);
}
class Small {
  constructor(tag, cls) { this.tag = tag; this.cls = cls || ""; this.kids = []; this.own = ""; this.disabled = false; }
  appendChild(child) { this.kids.push(child); return child; }
  get childNodes() { return this.kids; }
  addEventListener() {}
  setAttribute() {}
  get textContent() { return this.own + this.kids.map(k => k.textContent).join(""); }
  set textContent(value) { this.own = value; this.kids = []; }
  labels() { return [this.cls, ...this.kids.flatMap(k => k.labels())]; }
}
const smallEl = (tag, cls, text) => { const n = new Small(tag, cls); if (text != null) n.textContent = text; return n; };
const smallDoc = { createElement: tag => new Small(tag), createTextNode: text => smallEl("#text", null, text) };
const makeGuidance = (enabled) => new Function(
  "document", "el", "trackerURL", "actionsEnabled", "acknowledgeEnabled", "acknowledgePending",
  "isFinished", "acknowledgedAt", "fmtStamp", "buildResolveRow", "URL",
  script.slice(ackFrom, ackTo) + "\n" + script.slice(guidanceFrom, guidanceTo) + "\nreturn buildGuidance;")(
  smallDoc, smallEl, () => "", false, enabled, new Map(),
  vm.runInContext("isFinished", context), vm.runInContext("acknowledgedAt", context),
  vm.runInContext("fmtStamp", context), () => smallEl("div"), URL);

const guidance = makeGuidance(true);
let card = guidance(finished("g1", "failed", { finished_at: "2026-09-25T02:00:00Z", next_action: "確認してください" }));
assert.match(card.textContent, /この状態になった時刻\s*2026\/09\/25 11:00 JST/, card.textContent);
assert.ok(card.textContent.includes("確認して片付ける"), "no control on a card waiting to be cleared");
// The label the row is written with, so the sentence on the control that
// merely mentions clearing is not mistaken for a recorded time.
const clearedLabel = /片付けた時刻\s*\d{4}\//;
assert.ok(!clearedLabel.test(card.textContent), "a card nobody cleared claims a time for it: " + card.textContent);
console.log("PASS a finished card says when it got there and offers the control");

card = guidance(finished("g2", "done", { finished_at: "2026-09-25T02:00:00Z", acknowledged_at: "2026-09-25T07:45:00Z" }));
assert.match(card.textContent, /この状態になった時刻\s*2026\/09\/25 11:00 JST/, card.textContent);
assert.match(card.textContent, /片付けた時刻\s*2026\/09\/25 16:45 JST/, card.textContent);
assert.ok(clearedLabel.test(card.textContent), card.textContent);
assert.ok(!card.textContent.includes("確認して片付ける"), "a cleared card still offers to be cleared");
console.log("PASS a cleared card shows both times and no longer offers the control");

card = guidance(finished("g3", "stopped", {}));
assert.match(card.textContent, /この状態になった時刻\s*記録が残っていません/, card.textContent);
console.log("PASS a card whose ending was never written down says so rather than showing a blank");

card = makeGuidance(false)(finished("g4", "failed", { finished_at: "2026-09-25T02:00:00Z" }));
assert.ok(!card.textContent.includes("確認して片付ける"), "a read-only board offered a control it would refuse");
assert.ok(card.textContent.includes("閲覧専用"), card.textContent);
console.log("PASS a read-only board says why the card cannot be cleared");

const live = guidance(running("g5", "implement", 1));
assert.ok(!live.textContent.includes("この状態になった時刻") && !live.textContent.includes("確認して片付ける"),
  "a running card claims to have finished: " + live.textContent);
console.log("PASS a running card is offered neither a finish time nor the control");
