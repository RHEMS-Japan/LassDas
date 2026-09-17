// The live pane's behaviour, measured against the page's own code.
//
// Three defects lived here at once and no test could see any of them: a
// pinned pane gave itself up when the pointer crossed another ticket's
// rail, a pane opened with the keyboard could never be closed because a
// removed element never blurs, and a re-sorted board reopened a pane on a
// card nobody was pointing at. The page's script is read out of board.html
// and run against a DOM small enough to be obviously honest.
import { readFileSync } from "node:fs";

const html = readFileSync(process.argv[2], "utf8");
const script = [...html.matchAll(/<script>([\s\S]*?)<\/script>/g)].map(m => m[1]).join("\n");
const body = script.slice(script.indexOf("function deliveryIDOf"), script.indexOf("function fmtElapsed"));
if (!body) { console.log("FAIL harness: the live pane code was not found in board.html"); process.exit(1); }

function tokens(cls) {
  const set = new Set(cls ? cls.split(" ") : []);
  return { add: c => set.add(c), remove: c => set.delete(c), contains: c => set.has(c), has: c => set.has(c),
    toggle: (c, on) => (on === undefined ? (set.has(c) ? set.delete(c) : set.add(c)) : (on ? set.add(c) : set.delete(c))) };
}
function matches(node, part) {
  const attr = part.match(/\[data-([a-zA-Z-]+)="(.*)"\]/);
  const head = part.split("[")[0];
  if (head.startsWith(".")) { if (!node.classList.has(head.slice(1))) return false; }
  else if (head && head !== "*") { if (node.tag !== head) return false; }
  if (attr) { const key = attr[1].replace(/-([a-z])/g, (_, c) => c.toUpperCase()); return node.dataset[key] === attr[2]; }
  return true;
}
class Node {
  constructor(tag, cls) { this.tag = tag; this.classList = tokens(cls); this.dataset = {};
    this.children = []; this.text = ""; this.hovered = false; this.parent = null; this.handlers = {}; }
  appendChild(c) { c.parent = this; this.children.push(c); return c; }
  // Text of this node and everything under it, the way the page reads it
  // back after appending chunks.
  get textContent() { return this.text + this.children.map(c => c.textContent).join(""); }
  set textContent(v) { this.text = v; this.children = []; }
  matches(sel) { return sel === ":hover" ? this.hovered : false; }
  closest(sel) { let n = this; const want = sel.replace(".", "");
    while (n) { if (n.classList.has(want)) return n; n = n.parent; } return null; }
  all() { return [this, ...this.children.flatMap(c => c.all())]; }
  addEventListener(type, handler) { (this.handlers[type] = this.handlers[type] || []).push(handler); }
  click() { (this.handlers.click || []).forEach(h => h()); }
  querySelector(sel) { return this.querySelectorAll(sel)[0] || null; }
  querySelectorAll(sel) {
    let found = [this];
    for (const part of sel.trim().split(/\s+/)) {
      const next = [];
      for (const root of found) for (const n of root.all().slice(1)) if (matches(n, part)) next.push(n);
      found = [...new Set(next)];
    }
    return found;
  }
}
const el = (tag, cls, text) => { const n = new Node(tag, cls); if (text !== undefined) n.text = text; return n; };
// A clock and a network the checks drive by hand.
const pending = new Map();
let nextTimer = 1;
const setTimer = fn => { const id = nextTimer++; pending.set(id, fn); return id; };
const clearTimer = id => pending.delete(id);
const runTimers = () => { const due = [...pending]; pending.clear(); due.forEach(([, fn]) => fn()); };
let answer = () => Promise.resolve({ ok: true, json: () => Promise.resolve({ steps: [] }) });
const fetchStub = url => answer(url);
const live = new Function("CSS", "setTimeout", "clearTimeout", "fetch", "document", "el",
  body + "\n return { liveOpen, liveClose, liveDetach, liveRestore, wireLive, buildLivePane, liveTick," +
  " get follow() { return liveFollow; }, reset() { liveFollow = null; } };")(
  { escape: s => s }, setTimer, clearTimer, fetchStub,
  { createTextNode: t => { const n = new Node("#text"); n.text = t; return n; } }, el);
const settle = async () => { for (let i = 0; i < 20; i++) await Promise.resolve(); };

const STEPS = [["intake", "受付"], ["design", "設計"], ["implement", "実装"]];
function card(deliveryID, issueKey) {
  const c = new Node("div", "card");
  c.dataset.deliveryId = deliveryID; c.dataset.issueKey = issueKey;
  const pane = live.buildLivePane();
  const track = new Node("div", "track");
  for (const [id, name] of STEPS) {
    const node = new Node("div", "node"); node.dataset.step = id;
    live.wireLive(node, issueKey, id, name, pane); track.appendChild(node);
  }
  c.appendChild(track); c.appendChild(pane);
  return c;
}
const node = (c, stage) => c.querySelector('.node[data-step="' + stage + '"]');
const pane = c => c.querySelector(".livepane");
const fire = (n, type) => (n.handlers[type] || []).forEach(h => h());
const box = (...cards) => { const b = new Node("div", "runs"); cards.forEach(c => b.appendChild(c)); return b; };

let failed = 0;
function check(name, got, want) {
  if (got === want) { console.log("PASS " + name); return; }
  console.log("FAIL " + name + ": got " + JSON.stringify(got) + ", want " + JSON.stringify(want));
  failed++;
}

// A pinned pane is the reader saying "keep this open".
live.reset();
{
  const a = card("d1", "TICKET-1"), b = card("d2", "TICKET-2");
  box(a, b);
  node(a, "design").click();
  const crossed = node(b, "implement");
  fire(crossed, "mouseenter"); fire(crossed, "mouseleave");
  check("a pinned pane survives a pointer crossing another ticket's rail",
    live.follow && live.follow.deliveryID === "d1" && live.follow.pinned === true, true);
  check("and stays open", pane(a).classList.has("on"), true);
}

// A pane opened with the keyboard closes on the next refresh: the element
// it was opened from is gone, and a removed element never blurs.
live.reset();
{
  const a = card("d1", "TICKET-1");
  box(a);
  fire(node(a, "design"), "focus");
  check("the keyboard opens a pane", pane(a).classList.has("on"), true);
  const rebuilt = box(card("d1", "TICKET-1"));
  live.liveRestore(rebuilt);
  check("and a refresh closes it when the pointer is elsewhere",
    pane(rebuilt.querySelector(".card")).classList.has("on"), false);
}

// The board re-sorts. A pane must not open on a card nobody points at.
live.reset();
{
  const a = card("d1", "TICKET-1");
  box(a);
  node(a, "design").hovered = true;
  fire(node(a, "design"), "mouseenter");
  const moved = card("d1", "TICKET-1");
  const rebuilt = box(card("d2", "TICKET-2"), moved);
  live.liveRestore(rebuilt);
  check("no pane opens on a card the pointer left", pane(moved).classList.has("on"), false);
}

// Two deliveries of one ticket: the pane belongs to the one hovered.
live.reset();
{
  const first = card("d-old", "TICKET-1"), second = card("d-new", "TICKET-1");
  box(first, second);
  node(second, "design").hovered = true;
  fire(node(second, "design"), "mouseenter");
  const stillFirst = card("d-old", "TICKET-1"), stillSecond = card("d-new", "TICKET-1");
  node(stillSecond, "design").hovered = true;
  live.liveRestore(box(stillFirst, stillSecond));
  check("the older delivery's pane stays shut", pane(stillFirst).classList.has("on"), false);
  check("the hovered delivery's pane reopens", pane(stillSecond).classList.has("on"), true);
}

// A stage is several steps in a row. Moving to the next one must not empty
// the pane under a reader who pinned it.
live.reset();
{
  const a = card("d1", "TICKET-1");
  box(a);
  const p = pane(a);
  let step = "decide";
  answer = url => Promise.resolve(url.includes("/live/")
    ? { ok: true, json: () => Promise.resolve({ step, from: 0, next: 10, size: 10, text: step + " の出力\n" }) }
    : { ok: true, json: () => Promise.resolve({ steps: [{ step, stage: "design", bytes: 10, updated_at_ms: 1 }] }) });
  live.liveOpen(p, "TICKET-1", "design", "設計");
  await settle();
  const before = p.querySelector("pre").textContent;
  step = "apply";
  runTimers(); await settle();
  const after = p.querySelector("pre").textContent;
  check("what the reader was reading survives the stage moving on",
    after.includes("decide の出力"), true);
  check("and the new step says its name", after.includes("── apply ──"), true);
  if (!before.includes("decide")) { console.log("FAIL harness: the first step never rendered"); failed++; }
}

// A board that cannot be reached has not told us the stage produced
// nothing. Saying so puts the very sentence this pane removes in front of
// a reader whose network blinked.
live.reset();
{
  const a = card("d1", "TICKET-1");
  box(a);
  const p = pane(a);
  answer = () => Promise.reject(new Error("offline"));
  live.liveOpen(p, "TICKET-1", "design", "設計");
  await settle();
  check("an unreachable board does not claim the stage is empty",
    p.querySelector("pre").textContent.includes("まだありません"), false);
  check("and the pane keeps trying", pending.size > 0, true);
  answer = () => Promise.resolve({ ok: false, status: 500, json: () => Promise.resolve({}) });
  runTimers(); await settle();
  check("a failing board does not claim it either",
    p.querySelector("pre").textContent.includes("まだありません"), false);
}

process.exit(failed === 0 ? 0 : 1);
