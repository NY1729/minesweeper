import "./style.css";

type Cell = { x: number; y: number; value: number };
type ChunkSnapshot = {
  chunkX: number;
  chunkY: number;
  cells?: Cell[] | null;
  flags?: { i: number; o: string }[] | null;
  version: number;
};

type ServerMessage =
  | { type: "hello"; chunkSize: number; worldId: string }
  | { type: "chunk"; data: ChunkSnapshot }
  | { type: "reveal"; cells: Cell[] }
  | { type: "flag"; x: number; y: number; on: boolean; owner: string; version: number }
  | { type: "me"; id: string; pixels: string; name: string; score: number }
  | { type: "score"; me: number }
  | { type: "users"; users: Record<string, { p: string; n: string }> }
  | { type: "ranking"; top: { id: string; n: string; score: number }[] }
  | { type: "cursors"; cursors: { s: number; id: string; x: number; y: number }[] }
  | { type: "cursorbatch"; set: { s: number; id: string; x: number; y: number }[]; hide: number[] }
  | { type: "pong" };

const canvas = document.querySelector<HTMLCanvasElement>("#board")!;
const ctx = canvas.getContext("2d")!;
const flagBtn = document.querySelector<HTMLButtonElement>("#flag-btn")!;
const flagPreview = document.querySelector<HTMLImageElement>("#flag-preview")!;
const editor = document.querySelector<HTMLDialogElement>("#editor")!;
const pixelsEl = document.querySelector<HTMLCanvasElement>("#pixels")!;
const pctx = pixelsEl.getContext("2d")!;
const nameEl = document.querySelector<HTMLInputElement>("#name")!;
const rankList = document.querySelector<HTMLElement>("#rank-list")!;
const tipEl = document.querySelector<HTMLElement>("#tip")!;
const tipFlag = document.querySelector<HTMLCanvasElement>("#tip-flag")!;
const tipName = document.querySelector<HTMLElement>("#tip-name")!;
const rankMe = document.querySelector<HTMLElement>("#rank-me")!;

// A flag is 16x16 pixels, stored and sent as a string in one of two formats (told apart by length):
//  - 1024 chars: per pixel "a r g b", one hex digit each: RGB with 16 levels per channel
//    (4096 colors); alpha is 0 (transparent) or f (opaque). The editor writes this one.
//  - 256 chars (legacy): one base-36 digit per pixel indexing LEGACY_PALETTE, 0 = transparent.
//    Flags saved before free color selection still use it; never reorder LEGACY_PALETTE.
// In memory a pixel is -1 (transparent) or 0xRGB.
const LEGACY_PALETTE = [
  "", "#1f2533", "#ffffff", "#8a90a0", "#e0434f", "#f28c28", "#f4c430", "#a8d84e",
  "#2e9e57", "#13958c", "#7cc6f2", "#2f6fdf", "#6a4fd1", "#d94fa3", "#ffb3c1", "#8b5a2b",
  "#4a4f5c", "#c9cedb", "#8f1d2c", "#ff9e9e", "#c76a12", "#ffd99a", "#d9a066", "#fff1a8",
  "#5a7d1e", "#b8ecc8", "#1b5e3a", "#0b5c63", "#1c3d8f", "#b9cdfa", "#3a2a6b", "#c9b8f5",
];

const pack = (r: number, g: number, b: number) => (r << 8) | (g << 4) | b;
const rgb12 = (hex: string) => pack(...([1, 3, 5].map((i) => Math.round(parseInt(hex.slice(i, i + 2), 16) / 17)) as [number, number, number]));
const css12 = (v: number) => `rgb(${((v >> 8) & 15) * 17} ${((v >> 4) & 15) * 17} ${(v & 15) * 17})`;

function decodePixels(s: string): number[] {
  const px = new Array<number>(256).fill(-1);
  if (s.length === 1024) {
    for (let i = 0; i < 256; i++) {
      const v = parseInt(s.slice(i * 4 + 1, i * 4 + 4), 16);
      if (s[i * 4] !== "0" && !Number.isNaN(v)) px[i] = v;
    }
  } else if (s.length === 256) {
    for (let i = 0; i < 256; i++) {
      const c = LEGACY_PALETTE[parseInt(s[i], 36)];
      if (c) px[i] = rgb12(c);
    }
  }
  return px;
}

const encodePixels = (px: number[]) => px.map((v) => (v < 0 ? "0000" : "f" + v.toString(16).padStart(3, "0"))).join("");

function paintPx(target: CanvasRenderingContext2D, px: number[]) {
  const img = target.createImageData(16, 16);
  for (let i = 0; i < 256; i++) {
    const v = px[i];
    if (v >= 0) img.data.set([((v >> 8) & 15) * 17, ((v >> 4) & 15) * 17, (v & 15) * 17, 255], i * 4);
  }
  target.putImageData(img, 0, 0);
}

function makeImage(s: string): HTMLCanvasElement {
  const c = document.createElement("canvas");
  c.width = c.height = 16;
  paintPx(c.getContext("2d")!, decodePixels(s));
  return c;
}

// A pennant on a pole; also the flag everyone starts with (random color on the first visit).
function pennant(color: number): string {
  const px = new Array<number>(256).fill(-1);
  const pole = rgb12(LEGACY_PALETTE[1]);
  for (let y = 1; y < 15; y++) px[y * 16 + 3] = px[y * 16 + 4] = pole;
  for (let i = 0; i < 9; i++) {
    const w = (4 - Math.abs(i - 4)) * 2 + 1;
    for (let x = 0; x < w; x++) px[(2 + i) * 16 + 5 + x] = color;
  }
  return encodePixels(px);
}

const defaultFlag = makeImage(pennant(rgb12(LEGACY_PALETTE[3])));
const userImages = new Map<string, HTMLCanvasElement>();
const userNames = new Map<string, string>();
const displayName = (id: string) => userNames.get(id) || `Player ${id.slice(0, 4)}`;
const requestedUsers = new Set<string>();
let pendingUsers: string[] = [];

function wantUser(id: string) {
  if (!id || requestedUsers.has(id)) return;
  requestedUsers.add(id);
  pendingUsers.push(id);
}

// Batched: the server allows ~5 user lookups/sec, and chunk messages arrive in bursts.
let usersTimer: number | undefined;
function requestPendingUsers() {
  if (usersTimer !== undefined) return;
  usersTimer = window.setTimeout(() => {
    usersTimer = undefined;
    while (pendingUsers.length) send({ type: "users", ids: pendingUsers.splice(0, 64) });
  }, 200);
}

// The server accepts one profile save per 10s; send the latest one when allowed.
let lastProfileSave = -Infinity;
let profileSaveTimer: number | undefined;
function saveProfile() {
  clearTimeout(profileSaveTimer);
  const wait = lastProfileSave + 10_500 - Date.now();
  profileSaveTimer = window.setTimeout(() => {
    lastProfileSave = Date.now();
    send({ type: "setProfile", pixels: myPixels, name: myName });
  }, Math.max(0, wait));
}

function setUser(id: string, hex: string, name: string) {
  if (hex) userImages.set(id, makeImage(hex));
  userNames.set(id, name);
  if (id === myId) {
    myPixels = hex;
    myName = name;
    flagPreview.src = userImages.get(id)!.toDataURL();
  }
  renderRanking();
}

// Ranking (safe cells opened, -10 per mine): always shown. The server pushes the top 10
// whenever it changes (within ~2s); your own score arrives instantly with each reveal.
let ranking: { top: { id: string; n: string; score: number }[]; me: number } = { top: [], me: 0 };

function renderRanking() {
  rankList.replaceChildren(...ranking.top.map((r, i) => {
    const li = document.createElement("li");
    if (r.id === myId) li.className = "mine";
    const rank = document.createElement("span");
    rank.className = "rank";
    rank.textContent = String(i + 1);
    const img = document.createElement("img");
    img.alt = "";
    img.src = (userImages.get(r.id) ?? defaultFlag).toDataURL();
    const name = document.createElement("span");
    name.className = "name";
    name.textContent = r.n || displayName(r.id);
    const score = document.createElement("span");
    score.className = "score";
    score.textContent = r.score.toLocaleString();
    li.append(rank, img, name, score);
    return li;
  }));
  if (!ranking.top.length) {
    const li = document.createElement("li");
    li.className = "empty";
    li.textContent = "まだ誰もいません";
    rankList.append(li);
  }
  rankMe.textContent = ranking.me.toLocaleString();
}



let secret = "";
try { secret = localStorage.getItem("secret") ?? ""; } catch {}
if (!secret) {
  // getRandomValues, unlike randomUUID, also works over plain http (e.g. LAN dev).
  secret = Array.from(crypto.getRandomValues(new Uint8Array(16)), (b) => b.toString(16).padStart(2, "0")).join("");
  try { localStorage.setItem("secret", secret); } catch {}
}
let myId = "";
let myPixels = "";
let myName = "";

let editing: number[] = [];
let color = rgb12(LEGACY_PALETTE[4]);
type Tool = "pen" | "eraser" | "pick";
let tool: Tool = "pen";

let recent: number[] = [];
try {
  const saved = JSON.parse(localStorage.getItem("recentColors") ?? "[]");
  if (Array.isArray(saved)) recent = saved.filter((v) => Number.isInteger(v) && v >= 0 && v < 4096).slice(0, 10);
} catch {}

const PRESETS = [1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 21].map((i) => rgb12(LEGACY_PALETTE[i]));
const curEl = document.querySelector<HTMLElement>("#cur")!;
const curHex = document.querySelector<HTMLElement>("#cur-hex")!;
const recentEl = document.querySelector<HTMLElement>("#recent")!;
const recentGroup = document.querySelector<HTMLElement>("#recent-group")!;
const presetsEl = document.querySelector<HTMLElement>("#presets")!;
const channels = (["r", "g", "b"] as const).map((c) => ({
  input: document.querySelector<HTMLInputElement>(`#ch-${c}`)!,
  out: document.querySelector<HTMLElement>(`#out-${c}`)!,
}));
const toolBtns = [...document.querySelectorAll<HTMLButtonElement>("[data-tool]")];
const sizeBtns = [...document.querySelectorAll<HTMLButtonElement>("[data-size]")];
let size = 1; // pen / eraser width in pixels

function renderSwatches(el: HTMLElement, colors: number[]) {
  el.replaceChildren(...colors.map((v) => {
    const b = document.createElement("button");
    b.type = "button";
    b.style.background = css12(v);
    b.setAttribute("aria-label", `色 #${v.toString(16).padStart(3, "0")}`);
    b.setAttribute("aria-pressed", String(v === color));
    b.onclick = () => { setColor(v); setTool("pen"); };
    return b;
  }));
}

function setColor(v: number) {
  color = v;
  const ch = [(v >> 8) & 15, (v >> 4) & 15, v & 15];
  channels.forEach((c, i) => {
    c.input.value = String(ch[i]);
    c.out.textContent = String(ch[i]);
    // the track shows what this channel would do to the current color
    const lo = [...ch]; lo[i] = 0;
    const hi = [...ch]; hi[i] = 15;
    c.input.style.setProperty("--from", css12(pack(lo[0], lo[1], lo[2])));
    c.input.style.setProperty("--to", css12(pack(hi[0], hi[1], hi[2])));
  });
  curEl.style.background = css12(v);
  curHex.textContent = "#" + v.toString(16).padStart(3, "0");
  renderSwatches(presetsEl, PRESETS);
  renderSwatches(recentEl, recent);
}

function setTool(t: Tool) {
  tool = t;
  toolBtns.forEach((b) => b.setAttribute("aria-pressed", String(b.dataset.tool === t)));
}
toolBtns.forEach((b) => (b.onclick = () => setTool(b.dataset.tool as Tool)));
sizeBtns.forEach((b) => (b.onclick = () => {
  size = Number(b.dataset.size);
  sizeBtns.forEach((o) => o.setAttribute("aria-pressed", String(o === b)));
}));
channels.forEach((c) => (c.input.oninput = () => {
  setColor(pack(+channels[0].input.value, +channels[1].input.value, +channels[2].input.value));
  setTool("pen");
}));

function remember(v: number) {
  if (recent[0] === v) return;
  recent = [v, ...recent.filter((c) => c !== v)].slice(0, 10);
  try { localStorage.setItem("recentColors", JSON.stringify(recent)); } catch {}
  recentGroup.hidden = false;
  renderSwatches(recentEl, recent);
}

function paint(e: PointerEvent) {
  const r = pixelsEl.getBoundingClientRect();
  const x = Math.floor(((e.clientX - r.left) / r.width) * 16);
  const y = Math.floor(((e.clientY - r.top) / r.height) * 16);
  if (x < 0 || y < 0 || x > 15 || y > 15) return;
  const i = y * 16 + x;
  if (tool === "pick") {
    if (editing[i] >= 0) { setColor(editing[i]); setTool("pen"); }
    return;
  }
  // size x size square around the pointer (even sizes extend right/down), clipped at the edges
  const lo = -Math.floor((size - 1) / 2);
  for (let py = y + lo; py < y + lo + size; py++) {
    for (let px = x + lo; px < x + lo + size; px++) {
      if (px >= 0 && py >= 0 && px < 16 && py < 16) editing[py * 16 + px] = tool === "eraser" ? -1 : color;
    }
  }
  if (tool === "pen") remember(color);
  paintPx(pctx, editing);
}
pixelsEl.addEventListener("pointerdown", (e) => {
  pixelsEl.setPointerCapture(e.pointerId);
  paint(e);
});
pixelsEl.addEventListener("pointermove", (e) => {
  if (e.buttons & 1) paint(e);
});

flagBtn.onclick = () => {
  if (!myPixels) return;
  editing = decodePixels(myPixels);
  paintPx(pctx, editing);
  nameEl.value = myName;
  setTool("pen");
  recentGroup.hidden = recent.length === 0;
  setColor(color);
  editor.returnValue = ""; // Esc keeps the previous returnValue, which could be "save".
  editor.showModal();
};

editor.addEventListener("close", () => {
  if (editor.returnValue !== "save") return;
  setUser(myId, encodePixels(editing), nameEl.value.trim().slice(0, 16));
  saveProfile();
  draw();
});

let hover: { x: number; y: number } | null = null;
let pointer = { x: 0, y: 0 };
let tipShown = ""; // "owner|name" currently drawn in the tooltip, to redraw only on change

// Tooltip with the owner's name and flag while hovering a flagged cell (mouse only).
// Called after every draw (any state change) and on pointer moves (position only).
function updateTip() {
  const owner = hover && !dragging ? flags.get(key(hover.x, hover.y)) : undefined;
  if (owner === undefined) {
    tipEl.hidden = true;
    tipShown = "";
    return;
  }
  const name = owner ? displayName(owner) + (owner === myId ? "（あなた）" : "") : "持ち主なし";
  const shown = `${owner}|${name}|${userImages.get(owner) ? 1 : 0}`;
  if (shown !== tipShown) {
    tipShown = shown;
    const g = tipFlag.getContext("2d")!;
    g.clearRect(0, 0, 16, 16);
    g.drawImage(userImages.get(owner) ?? defaultFlag, 0, 0);
    tipName.textContent = name;
  }
  tipEl.hidden = false;
  // Keep inside the window: flip to the left of / above the pointer near the edges.
  const w = tipEl.offsetWidth;
  const h = tipEl.offsetHeight;
  const x = pointer.x + 14 + w > innerWidth ? pointer.x - 14 - w : pointer.x + 14;
  const y = pointer.y + 18 + h > innerHeight ? pointer.y - 18 - h : pointer.y + 18;
  tipEl.style.transform = `translate(${x}px, ${y}px)`;
}

// Cells other players are hovering, keyed by connection.
const cursors = new Map<number, { id: string; x: number; y: number }>();
const CURSOR_COLORS = [4, 5, 6, 8, 9, 11, 12, 13].map((i) => LEGACY_PALETTE[i]);
const cursorColor = (id: string) => CURSOR_COLORS[parseInt(id.slice(0, 8), 16) % CURSOR_COLORS.length];

// Own hovered cell: sent when it changes (at most every 100ms); the server relays it right away.
let myCursor: { x: number; y: number } | null = null;
let sentCursor = "";
let cursorTimer: number | undefined;
function queueCursor() {
  if (cursorTimer !== undefined) return;
  cursorTimer = window.setTimeout(() => {
    cursorTimer = undefined;
    const msg = myCursor
      ? { type: "cursor", px: myCursor.x, py: myCursor.y }
      : { type: "cursor", hide: true };
    const text = JSON.stringify(msg);
    if (text === sentCursor) return;
    sentCursor = text;
    send(msg);
  }, 100);
}
let longPressTimer: number | undefined;
let longPressed = false;

const DEFAULT_CHUNK_SIZE = 32;
let chunkSize = DEFAULT_CHUNK_SIZE;
let socket: WebSocket | null = null;
let reconnectTimer: number | undefined;

const revealed = new Map<number, number>();
const flags = new Map<number, string>(); // cell -> owner id

let centerX = 0;
let centerY = 0;
let cellSize = 28;
let dragging = false;
let dragMoved = false;
let pointerStartX = 0;
let pointerStartY = 0;
let centerStartX = 0;
let centerStartY = 0;
let lastSubscription = "";

// Numeric key (no string allocation per lookup; the draw loop does ~100k lookups a frame).
// ponytail: unique for |x|,|y| < 2^25 (~33M cells from the origin); widen if anyone gets there.
const key = (x: number, y: number) => (x + 2 ** 25) * 2 ** 26 + (y + 2 ** 25);
const floorDiv = (a: number, b: number) => Math.floor(a / b);
const mod = (a: number, b: number) => ((a % b) + b) % b;

function wsUrl(): string {
  const configured = import.meta.env.VITE_WS_URL as string | undefined;
  if (configured) return configured;
  const scheme = location.protocol === "https:" ? "wss:" : "ws:";
  return `${scheme}//${location.host}/api/ws`;
}

// Say so when the server cannot be reached (a silent blank board looks like a dead game).
// Shown only after a few seconds without a connection, and hidden again once connected.
const offlineEl = document.querySelector<HTMLElement>("#offline")!;
let offlineTimer: number | undefined;
function offlineSoon() {
  if (offlineTimer === undefined) offlineTimer = window.setTimeout(() => (offlineEl.hidden = false), 2500);
}
function online() {
  clearTimeout(offlineTimer);
  offlineTimer = undefined;
  offlineEl.hidden = true;
}

function connect() {
  clearTimeout(reconnectTimer);
  offlineSoon();

  socket = new WebSocket(wsUrl());

  socket.addEventListener("open", () => {
    online();
    send({ type: "auth", secret });
    send({ type: "ranking" });
    sentCursor = "";
    cursors.clear();
    // Retry user lookups whose replies were lost with the previous connection.
    for (const id of requestedUsers) if (!userImages.has(id)) pendingUsers.push(id);
    requestPendingUsers();
    subscribeVisibleChunks(true);
  });

  socket.addEventListener("message", (event) => {
    const msg = JSON.parse(event.data) as ServerMessage;

    if (msg.type === "hello") {
      chunkSize = msg.chunkSize;
      subscribeVisibleChunks(true);
      return;
    }

    if (msg.type === "me") {
      myId = msg.id;
      // Never ask the server for our own profile: while a save is still waiting out its
      // 10s rate limit, the server's copy is stale and would overwrite the new drawing.
      requestedUsers.add(myId);
      ranking.me = msg.score;
      if (msg.pixels) setUser(myId, msg.pixels, msg.name);
      else {
        // First visit: a pennant in a random palette color.
        setUser(myId, pennant(rgb12(LEGACY_PALETTE[4 + Math.floor(Math.random() * 12)])), msg.name);
        saveProfile();
      }
      draw();
      return;
    }

    if (msg.type === "users") {
      for (const [id, u] of Object.entries(msg.users)) setUser(id, u.p, u.n);
      draw();
      return;
    }

    if (msg.type === "score") {
      ranking.me = msg.me;
      renderRanking();
      return;
    }

    if (msg.type === "ranking") {
      ranking.top = msg.top;
      for (const r of msg.top) wantUser(r.id);
      requestPendingUsers();
      renderRanking();
      return;
    }

    // Selected cells that changed since the last batch (the server sends up to 10 batches a
    // second): `set` = new positions, `hide` = players who left or moved out of view.
    if (msg.type === "cursorbatch") {
      for (const c of msg.set) {
        cursors.set(c.s, { id: c.id, x: c.x, y: c.y });
        wantUser(c.id);
      }
      for (const s of msg.hide) cursors.delete(s);
      requestPendingUsers();
      draw();
      return;
    }

    // Full list for the current view, sent after each subscribe.
    if (msg.type === "cursors") {
      cursors.clear();
      for (const c of msg.cursors) {
        cursors.set(c.s, { id: c.id, x: c.x, y: c.y });
        wantUser(c.id);
      }
      requestPendingUsers();
      draw();
      return;
    }

    if (msg.type === "chunk") {
      applyChunk(msg.data);
      requestPendingUsers();
      draw();
      return;
    }

    if (msg.type === "reveal") {
      for (const cell of msg.cells) {
        revealed.set(key(cell.x, cell.y), cell.value);
        flags.delete(key(cell.x, cell.y));
      }
      draw();
      return;
    }

    if (msg.type === "flag") {
      const k = key(msg.x, msg.y);
      if (msg.on) {
        flags.set(k, msg.owner);
        wantUser(msg.owner);
        requestPendingUsers();
      } else flags.delete(k);
      draw();
    }
  });

  socket.addEventListener("close", () => {
    socket = null;
    reconnectTimer = window.setTimeout(connect, 1500);
  });

}

function applyChunk(chunk: ChunkSnapshot) {
  const baseX = chunk.chunkX * chunkSize;
  const baseY = chunk.chunkY * chunkSize;

  for (let ly = 0; ly < chunkSize; ly++) {
    for (let lx = 0; lx < chunkSize; lx++) {
      const k = key(baseX + lx, baseY + ly);
      revealed.delete(k);
      flags.delete(k);
    }
  }

  for (const cell of chunk.cells ?? []) {
    revealed.set(key(cell.x, cell.y), cell.value);
  }

  for (const f of chunk.flags ?? []) {
    const lx = f.i % chunkSize;
    const ly = Math.floor(f.i / chunkSize);
    flags.set(key(baseX + lx, baseY + ly), f.o);
    wantUser(f.o);
  }
}

function send(message: unknown) {
  if (socket?.readyState === WebSocket.OPEN) {
    socket.send(JSON.stringify(message));
  }
}

// The server allows ~5 subscribes/sec; while dragging, send at most every 250ms (trailing).
let lastSubscribeAt = 0;
let subscribeTimer: number | undefined;

function subscribeVisibleChunks(force = false) {
  if (!socket || socket.readyState !== WebSocket.OPEN) return;
  const wait = lastSubscribeAt + 250 - Date.now();
  if (wait > 0) {
    clearTimeout(subscribeTimer);
    subscribeTimer = window.setTimeout(() => subscribeVisibleChunks(force), wait);
    return;
  }

  const halfW = canvas.width / devicePixelRatio / 2 / cellSize;
  const halfH = canvas.height / devicePixelRatio / 2 / cellSize;
  const minX = Math.floor(centerX - halfW) - 2;
  const maxX = Math.ceil(centerX + halfW) + 2;
  const minY = Math.floor(centerY - halfH) - 2;
  const maxY = Math.ceil(centerY + halfH) + 2;

  const minCX = floorDiv(minX, chunkSize);
  const maxCX = floorDiv(maxX, chunkSize);
  const minCY = floorDiv(minY, chunkSize);
  const maxCY = floorDiv(maxY, chunkSize);

  const chunks: Array<{ x: number; y: number }> = [];
  for (let cy = minCY; cy <= maxCY; cy++) {
    for (let cx = minCX; cx <= maxCX; cx++) chunks.push({ x: cx, y: cy });
  }
  // The server sends the chunks in this order, so ask for the middle of the screen first:
  // what you look at appears before the edges. (If there were more than the 64 the server
  // accepts, it is the outer ones that get left out.)
  const ccx = centerX / chunkSize;
  const ccy = centerY / chunkSize;
  const dist = (c: { x: number; y: number }) => (c.x + 0.5 - ccx) ** 2 + (c.y + 0.5 - ccy) ** 2;
  chunks.sort((a, b) => dist(a) - dist(b));
  chunks.length = Math.min(chunks.length, 64);

  // The order changes as you move; only a change of the set of chunks needs a new subscription.
  const signature = chunks.map((c) => `${c.x},${c.y}`).sort().join("|");
  if (!force && signature === lastSubscription) return;
  lastSubscription = signature;
  lastSubscribeAt = Date.now();
  send({ type: "subscribe", chunks });
}

function worldFromScreen(screenX: number, screenY: number) {
  const width = canvas.width / devicePixelRatio;
  const height = canvas.height / devicePixelRatio;
  return {
    x: centerX + (screenX - width / 2) / cellSize,
    y: centerY + (screenY - height / 2) / cellSize,
  };
}

function cellFromScreen(screenX: number, screenY: number) {
  const p = worldFromScreen(screenX, screenY);
  return { x: Math.floor(p.x), y: Math.floor(p.y) };
}

function screenFromWorld(x: number, y: number) {
  const width = canvas.width / devicePixelRatio;
  const height = canvas.height / devicePixelRatio;
  return {
    x: (x - centerX) * cellSize + width / 2,
    y: (y - centerY) * cellSize + height / 2,
  };
}

const numberColor = [
  "#000000", "#2f6fdf", "#2e9e57", "#e0434f", "#6a4fd1",
  "#c76a12", "#13958c", "#333a48", "#8a90a0"
];

// Pointer events can arrive faster than the display refreshes; draw at most once per frame.
let frame = 0;
function draw() {
  if (frame) return;
  frame = requestAnimationFrame(() => {
    frame = 0;
    render();
  });
}

// Below this cell size, draw a simplified board (no text, grid or per-cell gaps).
const COARSE = 16;

function render() {
  const dpr = devicePixelRatio;
  const width = canvas.width / dpr;
  const height = canvas.height / dpr;

  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.clearRect(0, 0, width, height);
  const coarse = cellSize < COARSE;
  ctx.fillStyle = coarse ? "#d9e0ec" : "#eef1f7";
  ctx.fillRect(0, 0, width, height);

  const x0 = Math.floor(centerX - width / 2 / cellSize) - 1;
  const x1 = Math.ceil(centerX + width / 2 / cellSize) + 1;
  const y0 = Math.floor(centerY - height / 2 / cellSize) - 1;
  const y1 = Math.ceil(centerY + height / 2 / cellSize) + 1;
  const left = (x0 - centerX) * cellSize + width / 2;
  const top = (y0 - centerY) * cellSize + height / 2;

  ctx.imageSmoothingEnabled = false;
  ctx.font = `600 ${Math.max(11, cellSize * 0.42)}px sans-serif`;
  ctx.textAlign = "center";
  ctx.textBaseline = "middle";
  const flagSize = Math.round(cellSize * (coarse ? 0.9 : 0.75));
  const half = cellSize / 2;

  for (let y = y0, py = top; y <= y1; y++, py += cellSize) {
    for (let x = x0, px = left; x <= x1; x++, px += cellSize) {
      const k = key(x, y);
      const value = revealed.get(k);
      const owner = flags.get(k);

      // A flag marks its cell in the owner's color (same color as their hover cell).
      const mark = owner === undefined ? "" : owner ? cursorColor(owner) : "#8a90a0";

      if (coarse) {
        if (value === undefined && owner === undefined) continue; // background already unrevealed
        ctx.fillStyle = mark || (value === 9 ? "#ffd6dc" : "#ffffff");
        ctx.fillRect(px, py, cellSize + 0.5, cellSize + 0.5);
      } else {
        ctx.fillStyle = value === undefined ? "#d9e0ec" : value === 9 ? "#ffd6dc" : "#ffffff";
        ctx.fillRect(px + 1, py + 1, cellSize - 2, cellSize - 2);
        if (mark) {
          ctx.fillStyle = mark;
          ctx.globalAlpha = 0.22;
          ctx.fillRect(px + 1, py + 1, cellSize - 2, cellSize - 2);
          ctx.globalAlpha = 1;
          ctx.strokeStyle = mark;
          ctx.lineWidth = 2;
          ctx.strokeRect(px + 2, py + 2, cellSize - 4, cellSize - 4);
        }
      }

      if (owner !== undefined) {
        if (coarse && cellSize < 12) continue; // too small for the flag picture; the colored cell is the marker
        const img = userImages.get(owner) ?? defaultFlag;
        ctx.drawImage(img, px + (cellSize - flagSize) / 2, py + (cellSize - flagSize) / 2, flagSize, flagSize);
      } else if (!coarse && value !== undefined && value !== 0) {
        if (value === 9) {
          ctx.fillStyle = "#c0283c";
          ctx.fillText("✹", px + half, py + half);
        } else {
          ctx.fillStyle = numberColor[value] ?? "#fff";
          ctx.fillText(String(value), px + half, py + half);
        }
      }
    }
  }

  if (!coarse) {
    ctx.strokeStyle = "rgba(30, 40, 70, 0.06)";
    ctx.lineWidth = 1;
    ctx.beginPath();
    for (let x = x0, px = left; x <= x1; x++, px += cellSize) {
      ctx.moveTo(Math.round(px) + 0.5, 0);
      ctx.lineTo(Math.round(px) + 0.5, height);
    }
    for (let y = y0, py = top; y <= y1; y++, py += cellSize) {
      ctx.moveTo(0, Math.round(py) + 0.5);
      ctx.lineTo(width, Math.round(py) + 0.5);
    }
    ctx.stroke();
  }

  if (hover && !dragging) {
    const p = screenFromWorld(hover.x, hover.y);
    ctx.strokeStyle = "rgba(47, 111, 223, 0.7)";
    ctx.lineWidth = 2;
    ctx.strokeRect(p.x + 1, p.y + 1, cellSize - 2, cellSize - 2);
  }

  for (const c of cursors.values()) {
    const p = screenFromWorld(c.x, c.y);
    if (p.x < -cellSize || p.y < -cellSize || p.x > width || p.y > height) continue;
    const color = cursorColor(c.id);
    ctx.fillStyle = color;
    ctx.globalAlpha = 0.18;
    ctx.fillRect(p.x + 1, p.y + 1, cellSize - 2, cellSize - 2);
    ctx.globalAlpha = 1;
    ctx.strokeStyle = color;
    ctx.lineWidth = 2;
    ctx.strokeRect(p.x + 2, p.y + 2, cellSize - 4, cellSize - 4);

    // Name tag above the cell: [flag] name
    const label = displayName(c.id);
    ctx.font = "600 11px Inter, ui-sans-serif, system-ui, sans-serif";
    ctx.textAlign = "left";
    const tw = ctx.measureText(label).width;
    const tagW = tw + 26;
    const tagX = p.x + cellSize / 2 - tagW / 2;
    const tagY = p.y - 22;
    ctx.fillStyle = color;
    ctx.beginPath();
    ctx.roundRect(tagX, tagY, tagW, 18, 9);
    ctx.fill();
    ctx.imageSmoothingEnabled = false;
    ctx.drawImage(userImages.get(c.id) ?? defaultFlag, tagX + 4, tagY + 2, 14, 14);
    ctx.fillStyle = "#ffffff";
    ctx.textAlign = "left";
    ctx.textBaseline = "middle";
    ctx.fillText(label, tagX + 20, tagY + 9.5);
  }

  updateTip();
}

// The server accepts 64 subscribed chunks per client; zooming out further than this would
// leave part of the screen permanently unloaded (only the first 64 chunks are requested).
function minCellSize(): number {
  let cs = 10;
  while ((Math.ceil(innerWidth / cs / chunkSize) + 3) * (Math.ceil(innerHeight / cs / chunkSize) + 3) > 64) cs += 0.5;
  return cs;
}

function resize() {
  cellSize = Math.max(cellSize, minCellSize());
  const dpr = devicePixelRatio;
  canvas.width = Math.floor(innerWidth * dpr);
  canvas.height = Math.floor(innerHeight * dpr);
  canvas.style.width = `${innerWidth}px`;
  canvas.style.height = `${innerHeight}px`;
  draw();
  subscribeVisibleChunks();
}

let lastPointerType = "";

// Two-finger pinch: zoom around the point between the fingers and follow it, like a map.
const touches = new Map<number, { x: number; y: number }>();
let pinch: { dist: number; size: number; anchor: { x: number; y: number } } | null = null;

function pinchState() {
  const [a, b] = [...touches.values()];
  return { mid: { x: (a.x + b.x) / 2, y: (a.y + b.y) / 2 }, dist: Math.hypot(a.x - b.x, a.y - b.y) || 1 };
}

function startPinch() {
  const { mid, dist } = pinchState();
  pinch = { dist, size: cellSize, anchor: worldFromScreen(mid.x, mid.y) };
  dragging = false; // the first finger was starting a drag or a tap: neither any more
  dragMoved = true;
  clearTimeout(longPressTimer);
}

function applyPinch() {
  if (!pinch) return;
  const { mid, dist } = pinchState();
  cellSize = Math.min(64, Math.max(minCellSize(), (pinch.size * dist) / pinch.dist));
  // keep the world point that was between the fingers between them
  centerX = pinch.anchor.x - (mid.x - canvas.width / devicePixelRatio / 2) / cellSize;
  centerY = pinch.anchor.y - (mid.y - canvas.height / devicePixelRatio / 2) / cellSize;
}

// iOS Safari would otherwise zoom the whole page on a pinch.
document.addEventListener("gesturestart", (e) => e.preventDefault());

// Belt and braces for browsers that ignore user-select on a long press: no selection outside text fields.
document.addEventListener("selectstart", (e) => {
  const node = e.target as Node;
  const el = node instanceof Element ? node : node.parentElement; // a text node reports its parent
  if (!el?.closest("input, textarea")) e.preventDefault();
});
// ... and no context menu (Android) on the board or the buttons.
document.addEventListener("contextmenu", (e) => {
  if (!(e.target as Element).closest?.("input, textarea")) e.preventDefault();
});

canvas.addEventListener("pointerdown", (event) => {
  lastPointerType = event.pointerType;
  try { canvas.setPointerCapture(event.pointerId); } catch {} // the pointer may already be gone
  if (event.pointerType === "touch") {
    touches.set(event.pointerId, { x: event.clientX, y: event.clientY });
    if (touches.size === 2) { startPinch(); return; }
    if (touches.size > 2) return;
  }
  if (event.button !== 0) return;
  dragging = true;
  dragMoved = false;
  pointerStartX = event.clientX;
  pointerStartY = event.clientY;
  centerStartX = centerX;
  centerStartY = centerY;

  longPressed = false;
  if (event.pointerType === "touch") {
    longPressTimer = window.setTimeout(() => {
      if (dragMoved) return;
      longPressed = true;
      navigator.vibrate?.(20);
      const cell = cellFromScreen(pointerStartX, pointerStartY);
      send({ type: "flag", x: cell.x, y: cell.y });
    }, 450);
  }
});

canvas.addEventListener("pointermove", (event) => {
  if (event.pointerType === "touch" && touches.has(event.pointerId)) {
    touches.set(event.pointerId, { x: event.clientX, y: event.clientY });
    if (pinch) {
      applyPinch();
      draw();
      subscribeVisibleChunks();
      return;
    }
  }
  const cell = cellFromScreen(event.clientX, event.clientY);
  pointer = { x: event.clientX, y: event.clientY };
  if (event.pointerType === "mouse") {
    myCursor = cell;
    queueCursor();
  }

  if (!dragging) {
    if (event.pointerType !== "touch" && (hover?.x !== cell.x || hover?.y !== cell.y)) {
      hover = cell;
      draw();
    }
    if (event.pointerType === "mouse") updateTip();
    return;
  }
  const dx = event.clientX - pointerStartX;
  const dy = event.clientY - pointerStartY;

  if (Math.abs(dx) + Math.abs(dy) > 4) {
    dragMoved = true;
    clearTimeout(longPressTimer);
  }

  centerX = centerStartX - dx / cellSize;
  centerY = centerStartY - dy / cellSize;
  draw();
  subscribeVisibleChunks();
});

canvas.addEventListener("pointerup", (event) => {
  if (event.pointerType === "touch") {
    touches.delete(event.pointerId);
    if (pinch) {
      if (touches.size < 2) pinch = null; // a finger left: the other one must not start a drag or a tap
      return;
    }
  }
  if (!dragging) return;
  dragging = false;
  try { canvas.releasePointerCapture(event.pointerId); } catch {}
  clearTimeout(longPressTimer);

  if (!dragMoved && !longPressed) {
    const cell = cellFromScreen(event.clientX, event.clientY);
    send({ type: "reveal", x: cell.x, y: cell.y });
  }
});

canvas.addEventListener("pointercancel", () => {
  touches.clear();
  pinch = null;
  dragging = false;
  clearTimeout(longPressTimer);
});

canvas.addEventListener("contextmenu", (event) => {
  event.preventDefault();
  // Touch long-press also fires contextmenu; the long-press timer already flagged it.
  if (lastPointerType === "touch") return;
  const cell = cellFromScreen(event.clientX, event.clientY);
  send({ type: "flag", x: cell.x, y: cell.y });
});

canvas.addEventListener("wheel", (event) => {
  event.preventDefault();

  const before = worldFromScreen(event.clientX, event.clientY);
  const scale = Math.exp(-event.deltaY * 0.0012);
  cellSize = Math.min(64, Math.max(minCellSize(), cellSize * scale));
  const after = worldFromScreen(event.clientX, event.clientY);

  centerX += before.x - after.x;
  centerY += before.y - after.y;
  if (myCursor) {
    myCursor = cellFromScreen(event.clientX, event.clientY);
    queueCursor();
  }

  draw();
  subscribeVisibleChunks();
}, { passive: false });

canvas.addEventListener("pointerleave", () => {
  myCursor = null;
  queueCursor();
  hover = null;
  draw();
});

window.addEventListener("resize", resize);

resize();
connect();
