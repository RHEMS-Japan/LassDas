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
// The live pane's own code, from the first of its declarations to the one
// after it. Every name the checks drive is required to be inside the slice:
// a declaration moved out of it would otherwise be silently untested, which
// is how a harness stops testing the thing it names (review of #200).
const from = script.indexOf("let pointerAt");
const to = script.indexOf("function fmtElapsed");
const body = from < 0 || to < 0 || to < from ? "" : script.slice(from, to);
const required = [
  "pointerIsOver", "closeOtherPanes", "deliveryIDOf", "buildLivePane", "liveStop",
  "liveFollow", "liveClose", "liveDetach", "liveRestore", "liveOpen", "liveTick", "wireLive",
];
const missing = required.filter(name => !body.includes(name === "liveFollow" ? "let liveFollow" : "function " + name));
if (!body || missing.length) {
  console.log("FAIL harness: the live pane code is not all in one place; missing " + JSON.stringify(missing));
  process.exit(1);
}

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
  contains(other) { let n = other; while (n) { if (n === this) return true; n = n.parent; } return false; }
  replaceChildren(...next) { this.children.forEach(c => (c.parent = null)); this.children = []; next.forEach(c => this.appendChild(c)); }
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
// Between scenarios: every pane stopped, every timer dropped, the follow
// cleared. Without this a pane opened by an earlier block kept ticking
// against the network the current block had just replaced, and a check
// could pass or fail because of the order the blocks happen to be in
// (review of #200).
const startScenario = () => {
  for (const pane of runsBox.querySelectorAll(".livepane")) live.liveDetach(pane);
  pending.clear();
  live.reset();
  runsBox.replaceChildren();
};
let answer = () => Promise.resolve({ ok: true, json: () => Promise.resolve({ steps: [] }) });
const fetchStub = url => answer(url);
// The page asks the document where the pointer is, by coordinate. The
// harness answers by hit-test against a node the check nominates, which is
// the shape of the real API: it is asked after the tree changed and it
// answers about the tree as it is now.
// The pointer is a position, not a node: elementFromPoint answers with
// whatever is at that position in the tree as it is now. Modelling it as a
// flag on a node let the check assign the answer it wanted after a rebuild
// (review of #200).
let pointerY = null;
const listeners = {};
const bodyNode = new Node("body");
const documentStub = {
  body: bodyNode,
  createTextNode: t => { const n = new Node("#text"); n.text = t; return n; },
  addEventListener: (type, fn) => { (listeners[type] = listeners[type] || []).push(fn); },
  getElementById: id => registry[id] || null,
  // Resolved by position: which card is at that place on the board now,
  // and which rail node within it. Resolving by delivery instead made the
  // pointer follow a card wherever it moved, which no pointer does - and
  // the one case that matters, cards re-sorting under a pointer that did
  // not move, could not be written down at all (review of #200).
  // Resolved by coordinate against the board as it is laid out now. Cards
  // have heights, so one growing taller moves everything below it and what
  // is under a pointer that did not move changes - which is what a browser
  // does and what a model keyed on "which card" could not say (review of
  // #200).
  elementFromPoint: () => {
    if (pointerY === null) return null;
    let top = 0;
    for (const card of runsBox.children) {
      const height = card.height === undefined ? 100 : card.height;
      if (pointerY >= top && pointerY < top + height) {
        // The rail sits in the first twenty of a card's height, one node
        // per stage; below it is the pane and the rest of the card.
        if (pointerY - top >= 20) return card.querySelector(".livepane") || card;
        const nodes = card.querySelectorAll(".node");
        return nodes[Math.min(nodes.length - 1, Math.floor((pointerY - top) / 20 * nodes.length))] || card;
      }
      top += height;
    }
    return null;
  },
};
const registry = {};
// Move the pointer over the rail node of whichever card sits at that place
// on the board, or off the board entirely. The pointer stays where it is
// put; what is under it is whatever the board puts there.
// Put the pointer at a height on the board, or off it. What is under it is
// whatever the board has laid out there.
const movePointerToY = y => {
  pointerY = y;
  (listeners.mousemove || []).forEach(fn => fn({ clientX: 1, clientY: y || 0 }));
};
// The rail node of the card at a place, expressed as a height: the nth
// card's rail, at the slice belonging to one stage.
const railY = (slot, stageIndex) => slot * 100 + Math.floor((stageIndex + 0.5) * 20 / STEPS.length);
const live = new Function("CSS", "setTimeout", "clearTimeout", "fetch", "document", "el",
  body + "\n return { liveOpen, liveClose, liveDetach, liveRestore, wireLive, buildLivePane, liveTick, closeOtherPanes," +
  " get follow() { return liveFollow; }, reset() { liveFollow = null; } };")(
  { escape: s => s }, setTimer, clearTimer, fetchStub, documentStub, el);
const settle = async () => { for (let i = 0; i < 20; i++) await Promise.resolve(); };

const STEPS = [["intake", "受付"], ["design", "設計"], ["implement", "実装"]];
function card(deliveryID, issueKey) {
  const c = new Node("div", "card");
  c.dataset.delivery = deliveryID; c.dataset.issueKey = issueKey;
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
// The board's own container, registered under the id the page looks it up
// by, and rebuilt the way renderBoard rebuilds it.
const runsBox = new Node("div", "runs");
registry.runs = runsBox;
const box = (...cards) => { runsBox.replaceChildren(...cards); return runsBox; };
const setHeights = (...heights) => { runsBox.children.forEach((c, i) => (c.height = heights[i])); };
// What renderBoard does, in its order: stop every pane, replace the cards,
// then restore.
const rebuild = (...cards) => {
  for (const pane of runsBox.querySelectorAll(".livepane")) live.liveDetach(pane);
  runsBox.replaceChildren(...cards);
  live.liveRestore(runsBox);
  return runsBox;
};

let failed = 0;
let checks = 0;
function check(name, got, want) {
  checks++;
  if (got === want) { console.log("PASS " + name); return; }
  console.log("FAIL " + name + ": got " + JSON.stringify(got) + ", want " + JSON.stringify(want));
  failed++;
}

// A pinned pane is the reader saying "keep this open".
startScenario();
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
startScenario();
{
  const a = card("d1", "TICKET-1");
  box(a);
  fire(node(a, "design"), "focus");
  check("the keyboard opens a pane", pane(a).classList.has("on"), true);
  movePointerToY(null);
  const survivor = card("d1", "TICKET-1");
  rebuild(survivor);
  check("and a refresh closes it when the pointer is elsewhere",
    pane(survivor).classList.has("on"), false);
}

// The board re-sorts. A pane must not open on a card nobody points at.
startScenario();
{
  const a = card("d1", "TICKET-1");
  box(a);
  movePointerToY(railY(0, 1));
  fire(node(a, "design"), "mouseenter");
  // The pointer does not move; the board re-sorts under it. What is at
  // that place is now another ticket's rail, so the pane that was open
  // does not come back on the card that moved away from the pointer.
  const moved = card("d1", "TICKET-1");
  rebuild(card("d2", "TICKET-2"), moved);
  check("no pane opens on a card that moved out from under the pointer",
    pane(moved).classList.has("on"), false);
  const arrived = runsBox.children[0];
  check("and none opens on the card that took its place",
    pane(arrived).classList.has("on"), false);
}

// Two deliveries of one ticket: the pane belongs to the one hovered.
startScenario();
{
  const first = card("d-old", "TICKET-1"), second = card("d-new", "TICKET-1");
  box(first, second);
  movePointerToY(railY(1, 1));
  fire(node(second, "design"), "mouseenter");
  const stillFirst = card("d-old", "TICKET-1"), stillSecond = card("d-new", "TICKET-1");
  rebuild(stillFirst, stillSecond);
  rebuild(stillFirst, stillSecond);
  check("the older delivery's pane stays shut", pane(stillFirst).classList.has("on"), false);
  check("the hovered delivery's pane reopens", pane(stillSecond).classList.has("on"), true);
}

// A stage is several steps in a row. Moving to the next one must not empty
// the pane under a reader who pinned it - and the next step has usually
// written nothing yet when it starts, which is when the pane used to give
// up the reader's text and claim the stage was empty.
startScenario();
{
  const a = card("d1", "TICKET-1");
  box(a);
  const p = pane(a);
  // What each step has written, and which step is newest.
  const written = { decide: "決定: 収束\n", apply: "" };
  let newest = "decide";
  answer = url => {
    const asked = url.match(/\/live\/([^?]+)\?from=(\d+)/);
    if (!asked) {
      return Promise.resolve({ ok: true, json: () => Promise.resolve({
        steps: Object.keys(written).map((step, index) => ({
          step, stage: "design", bytes: written[step].length,
          updated_at_ms: step === newest ? 100 : index,
        })),
      }) });
    }
    const step = decodeURIComponent(asked[1]), from = Number(asked[2]);
    const all = written[step] || "";
    return Promise.resolve({ ok: true, json: () => Promise.resolve({
      step, from, next: all.length, size: all.length, text: all.slice(from),
    }) });
  };
  live.liveOpen(p, "TICKET-1", "design", "設計");
  await settle();
  check("the first step's output is shown", p.querySelector("pre").textContent.includes("決定: 収束"), true);

  // decide finishes and apply becomes newest, with nothing written yet.
  newest = "apply";
  runTimers(); await settle();   // notices decide added nothing
  runTimers(); await settle();   // picks up apply
  runTimers(); await settle();   // and again, with apply still silent
  const quiet = p.querySelector("pre").textContent;
  check("what the reader was reading survives the stage moving on", quiet.includes("決定: 収束"), true);
  check("and the new step says its name", quiet.includes("── apply ──"), true);
  check("and the pane does not claim the stage is empty", quiet.includes("まだありません"), false);

  // apply writes, and then the stage hands back to a step already read.
  written.apply = "候補を適用します\n";
  runTimers(); await settle();
  newest = "decide";
  runTimers(); await settle();
  runTimers(); await settle();
  const back = p.querySelector("pre").textContent;
  const times = back.split("決定: 収束").length - 1;
  check("a step already read is not pasted a second time", times, 1);
}

// A board that cannot be reached has not told us the stage produced
// nothing. Saying so puts the very sentence this pane removes in front of
// a reader whose network blinked.
startScenario();
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

// Unpinning puts the pane away for good. Leaving the follow pinned made it
// come back, pinned, on the next refresh - so the reader could never put it
// away at all.
startScenario();
{
  const a = card("d1", "TICKET-1");
  box(a);
  const design = node(a, "design");
  design.click();
  check("a click pins the pane", pane(a).state.pinned, true);
  design.click();
  check("a second click closes it", pane(a).classList.has("on"), false);
  movePointerToY(null);
  const after = card("d1", "TICKET-1");
  rebuild(after);
  check("and it stays closed", pane(after).classList.has("on"), false);
}

// One pane at a time. Two open panes each poll and each say 固定中, and the
// next refresh drops one of them without the reader asking.
startScenario();
{
  const a = card("d1", "TICKET-1"), b = card("d2", "TICKET-2");
  box(a, b);
  node(a, "design").click();
  node(b, "implement").click();
  const open = runsBox.querySelectorAll(".livepane").filter(p => p.classList.has("on"));
  check("pinning a second pane closes the first", open.length, 1);
  check("and the one left open is the one just pinned", open[0] === pane(b), true);
}

// A pinned pane is read across refreshes, which happen about once a
// minute. It used to go back to 読み込み中… and re-read its log from the
// beginning every time, losing the reader's place.
startScenario();
{
  const a = card("d1", "TICKET-1");
  box(a);
  const askedFrom = [];
  const text = "検証 1 行目\n検証 2 行目\n検証 3 行目\n";
  answer = url => {
    if (!url.includes("/live/")) {
      return Promise.resolve({ ok: true, json: () => Promise.resolve({
        steps: [{ step: "run-validation", stage: "design", bytes: text.length, updated_at_ms: 1 }] }) });
    }
    const from = Number(url.match(/from=(\d+)/)[1]);
    askedFrom.push(from);
    return Promise.resolve({ ok: true, json: () => Promise.resolve({
      step: "run-validation", from, next: text.length, size: text.length, text: text.slice(from) }) });
  };
  node(a, "design").click();
  await settle();
  check("a pinned pane shows its step's output", pane(a).querySelector("pre").textContent.includes("検証 3 行目"), true);
  const readsBefore = askedFrom.length;

  movePointerToY(null);
  const after = card("d1", "TICKET-1");
  rebuild(after);
  await settle();
  check("and still shows it the moment the board refreshes",
    pane(after).querySelector("pre").textContent.includes("検証 3 行目"), true);
  check("without going back to 読み込み中",
    pane(after).querySelector("pre").textContent.includes("読み込み中"), false);
  runTimers(); await settle();
  check("and does not read the log from its beginning again",
    askedFrom.slice(readsBefore).some(from => from === 0), false);
}

// A card growing taller moves everything below it. The pointer did not
// move, so what is under it is no longer the rail it was over.
startScenario();
{
  const a = card("d1", "TICKET-1"), b = card("d2", "TICKET-2");
  box(a, b);
  setHeights(100, 100);
  movePointerToY(railY(1, 1));
  fire(node(b, "design"), "mouseenter");
  check("the second card's pane opens", pane(b).classList.has("on"), true);
  // The board's own order: stop the panes, put the new cards in, lay them
  // out, then restore. The card above has grown, so the pointer that did
  // not move is no longer over the rail it was over.
  for (const p of runsBox.querySelectorAll(".livepane")) live.liveDetach(p);
  const grown = card("d1", "TICKET-1"), moved = card("d2", "TICKET-2");
  runsBox.replaceChildren(grown, moved);
  setHeights(180, 100);
  live.liveRestore(runsBox);
  check("and closes when the card above it grows and pushes it away from the pointer",
    pane(moved).classList.has("on"), false);
}

// The log grew past what one answer carries while the tab was in the
// background. Two lines that were never next to each other must not be
// shown as though they were.
startScenario();
{
  const a = card("d1", "TICKET-1");
  box(a);
  let served = 0;
  answer = url => {
    if (!url.includes("/live/")) {
      return Promise.resolve({ ok: true, json: () => Promise.resolve({
        steps: [{ step: "s", stage: "design", bytes: 70010, updated_at_ms: 1 }] }) });
    }
    served++;
    return Promise.resolve({ ok: true, json: () => Promise.resolve(served === 1
      ? { step: "s", from: 0, next: 10, size: 10, text: "はじめの行\n" }
      : { step: "s", from: 70000, next: 70010, size: 70010, text: "ずっと後の行\n" }) });
  };
  live.liveOpen(pane(a), "TICKET-1", "design", "設計");
  await settle();
  runTimers(); await settle();
  const shown = pane(a).querySelector("pre").textContent;
  check("a skipped stretch of log says so", shown.includes("中略"), true);
  check("and both lines are still there", shown.includes("はじめの行") && shown.includes("ずっと後の行"), true);
}

if (checks < 29) {
  console.log("FAIL harness: only " + checks + " checks ran; something stopped them early");
  failed++;
}
process.exit(failed === 0 ? 0 : 1);
