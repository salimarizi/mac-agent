'use strict';
// Mac Remote's bridge into a VS Code-family editor (VS Code, Cursor, Antigravity).
// Nothing outside the editor can read its terminals, so this keeps a headless copy
// of each one and serves it to the Mac Remote agent on a token-protected local
// port. The agent finds the port through a discovery file per editor window.

const vscode = require('vscode');
const http = require('http');
const crypto = require('crypto');
const fs = require('fs');
const os = require('os');
const path = require('path');
const { Terminal: Headless } = require('@xterm/headless');

const DISCOVERY_DIR = path.join(os.homedir(), 'Library', 'Application Support', 'MacRemote', 'editors');
const SCROLLBACK = 5000;
const TAIL_ROWS = 60;
const MAX_BODY_BYTES = 64 * 1024;
const MAX_SEND_BYTES = 8 * 1024;
const DEFAULT_COLS = 120;
const DEFAULT_ROWS = 30;

const mirrors = new Map(); // vscode.Terminal → mirror
let nextId = 1;
let discoveryFile = null;

function mirrorFor(terminal) {
  let m = mirrors.get(terminal);
  if (!m) {
    let cols = DEFAULT_COLS;
    let rows = DEFAULT_ROWS;
    try {
      const d = terminal.dimensions;
      if (d && d.columns > 0 && d.rows > 0) { cols = d.columns; rows = d.rows; }
    } catch { /* not exposed by this editor version */ }
    m = {
      id: String(nextId++),
      terminal,
      xterm: new Headless({ cols, rows, scrollback: SCROLLBACK, allowProposedApi: true }),
      version: 0,
      running: false,
    };
    mirrors.set(terminal, m);
  }
  return m;
}

function feed(m, data) {
  m.xterm.write(data);
  m.version++;
}

// watchOutput mirrors everything a terminal prints when the editor allows the
// terminalDataWriteEvent proposal, and otherwise falls back to shell
// integration, which only sees the output of commands started from now on.
function watchOutput(context) {
  let raw = false;
  try {
    const onData = vscode.window.onDidWriteTerminalData;
    if (typeof onData === 'function') {
      context.subscriptions.push(onData(e => feed(mirrorFor(e.terminal), e.data)));
      raw = true;
    }
  } catch { /* proposal not enabled for this extension */ }

  context.subscriptions.push(vscode.window.onDidStartTerminalShellExecution(async e => {
    const m = mirrorFor(e.terminal);
    m.running = true;
    m.version++;
    if (raw) return;
    feed(m, `\x1b[1;35m❯\x1b[0m \x1b[1m${e.execution.commandLine.value}\x1b[0m\r\n`);
    try {
      for await (const data of e.execution.read()) feed(m, data);
    } catch (err) {
      console.error('mac-remote: reading terminal output failed', err);
    }
  }));

  context.subscriptions.push(vscode.window.onDidEndTerminalShellExecution(e => {
    const m = mirrorFor(e.terminal);
    m.running = false;
    m.version++;
    if (raw) return;
    if (m.xterm.buffer.active.cursorX > 0) feed(m, '\r\n');
    if (e.exitCode) feed(m, `\x1b[31m✗ exit ${e.exitCode}\x1b[0m\r\n`);
  }));

  try {
    context.subscriptions.push(vscode.window.onDidChangeTerminalDimensions(e => {
      const { columns, rows } = e.dimensions;
      if (columns > 0 && rows > 0) mirrorFor(e.terminal).xterm.resize(columns, rows);
    }));
  } catch { /* not exposed by this editor version */ }

  context.subscriptions.push(vscode.window.onDidCloseTerminal(t => {
    const m = mirrors.get(t);
    if (m) { m.xterm.dispose(); mirrors.delete(t); }
  }));

  return raw;
}

// ── Reading a mirror back as lines ──────────────────────────────────────────
// Lines carry SGR escape codes so the phone can show the terminal's real
// colours; rows the terminal soft-wrapped are joined back into one line.

function sgrFor(cell) {
  const p = [];
  if (cell.isBold()) p.push(1);
  if (cell.isDim()) p.push(2);
  if (cell.isItalic()) p.push(3);
  if (cell.isUnderline()) p.push(4);
  if (cell.isInverse()) p.push(7);
  if (cell.isFgPalette()) {
    const c = cell.getFgColor();
    p.push(c < 8 ? 30 + c : c < 16 ? 82 + c : `38;5;${c}`);
  } else if (cell.isFgRGB()) {
    const c = cell.getFgColor();
    p.push(`38;2;${(c >> 16) & 255};${(c >> 8) & 255};${c & 255}`);
  }
  if (cell.isBgPalette()) {
    const c = cell.getBgColor();
    p.push(c < 8 ? 40 + c : c < 16 ? 92 + c : `48;5;${c}`);
  } else if (cell.isBgRGB()) {
    const c = cell.getBgColor();
    p.push(`48;2;${(c >> 16) & 255};${(c >> 8) & 255};${c & 255}`);
  }
  return p.join(';');
}

// A trailing space only matters if it is visible: a background, inverse or underline.
function visibleSpace(cell) {
  return !cell.isBgDefault() || cell.isInverse() || cell.isUnderline();
}

function serialize(cells) {
  let end = cells.length;
  while (end > 0 && cells[end - 1].ch === ' ' && !cells[end - 1].visible) end--;
  let out = '';
  let cur = '';
  for (let i = 0; i < end; i++) {
    const { ch, sgr } = cells[i];
    if (sgr !== cur) {
      out += sgr ? `\x1b[0;${sgr}m` : '\x1b[0m';
      cur = sgr;
    }
    out += ch;
  }
  if (cur) out += '\x1b[0m';
  return out;
}

function readLines(m, fromRow) {
  const buf = m.xterm.buffer.active;
  const cell = buf.getNullCell();
  let y = Math.max(0, fromRow);
  while (y > 0 && buf.getLine(y)?.isWrapped) y--;
  const lines = [];
  let cells = null;
  for (; y < buf.length; y++) {
    const line = buf.getLine(y);
    if (!line) continue;
    if (!line.isWrapped) {
      if (cells) lines.push(serialize(cells));
      cells = [];
    }
    for (let x = 0; x < line.length; x++) {
      line.getCell(x, cell);
      if (cell.getWidth() === 0) continue;
      cells.push({ ch: cell.getChars() || ' ', sgr: sgrFor(cell), visible: visibleSpace(cell) });
    }
  }
  if (cells) lines.push(serialize(cells));
  while (lines.length && lines[lines.length - 1] === '') lines.pop();
  return lines;
}

function stripSgr(line) {
  return line.replace(/\x1b\[[0-9;]*m/g, '');
}

// ── Local server for the agent ──────────────────────────────────────────────

function sendJSON(res, status, body) {
  res.writeHead(status, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' });
  res.end(JSON.stringify(body));
}

function authorized(req, token) {
  const got = Buffer.from(req.headers.authorization || '');
  const want = Buffer.from(`Bearer ${token}`);
  return got.length === want.length && crypto.timingSafeEqual(got, want);
}

function readBody(req) {
  return new Promise((resolve, reject) => {
    let size = 0;
    const chunks = [];
    req.on('data', c => {
      size += c.length;
      if (size > MAX_BODY_BYTES) { reject(new Error('body too large')); req.destroy(); return; }
      chunks.push(c);
    });
    req.on('end', () => {
      try { resolve(JSON.parse(Buffer.concat(chunks).toString('utf8') || '{}')); } catch (err) { reject(err); }
    });
    req.on('error', reject);
  });
}

function findMirror(id) {
  for (const t of vscode.window.terminals) {
    const m = mirrorFor(t);
    if (m.id === id) return m;
  }
  return null;
}

async function listTerminals() {
  const out = [];
  for (const t of vscode.window.terminals) {
    const m = mirrorFor(t);
    let pid = 0;
    try { pid = (await t.processId) || 0; } catch { /* process already gone */ }
    const buf = m.xterm.buffer.active;
    let cwd = '';
    try { cwd = t.shellIntegration?.cwd?.fsPath || ''; } catch { /* no shell integration */ }
    out.push({
      id: m.id,
      name: t.name,
      pid,
      cwd,
      running: m.running,
      version: m.version,
      tail: readLines(m, buf.length - TAIL_ROWS).map(stripSgr),
    });
  }
  return out;
}

async function handle(req, res, token) {
  if (!authorized(req, token)) return sendJSON(res, 401, { error: 'unauthorized' });
  const url = new URL(req.url, 'http://127.0.0.1');
  const parts = url.pathname.split('/').filter(Boolean);
  try {
    if (req.method === 'GET' && parts.length === 1 && parts[0] === 'terminals') {
      return sendJSON(res, 200, {
        app: vscode.env.appName,
        window: vscode.workspace.name || '',
        terminals: await listTerminals(),
      });
    }
    if (parts[0] !== 'terminals' || !parts[1]) return sendJSON(res, 404, { error: 'not_found' });
    const m = findMirror(parts[1]);
    if (!m) return sendJSON(res, 404, { error: 'not_found' });

    if (req.method === 'GET' && parts.length === 2) {
      return sendJSON(res, 200, { lines: readLines(m, 0) });
    }
    if (req.method === 'POST' && parts[2] === 'send') {
      const body = await readBody(req);
      const text = typeof body.text === 'string' ? body.text : '';
      if (Buffer.byteLength(text) > MAX_SEND_BYTES) return sendJSON(res, 413, { error: 'too_long' });
      m.terminal.sendText(text, true);
      return sendJSON(res, 200, { ok: true });
    }
    if (req.method === 'POST' && parts[2] === 'interrupt') {
      m.terminal.sendText('\x03', false);
      return sendJSON(res, 200, { ok: true });
    }
    return sendJSON(res, 404, { error: 'not_found' });
  } catch (err) {
    console.error('mac-remote: request failed', err);
    return sendJSON(res, 500, { error: 'failed', message: String(err && err.message || err) });
  }
}

function writeDiscovery(port, token, raw) {
  fs.mkdirSync(DISCOVERY_DIR, { recursive: true, mode: 0o700 });
  discoveryFile = path.join(DISCOVERY_DIR, `${process.pid}.json`);
  const body = JSON.stringify({
    pid: process.pid,
    port,
    token,
    app: vscode.env.appName,
    window: vscode.workspace.name || '',
    fullOutput: raw,
  });
  fs.writeFileSync(discoveryFile, body, { mode: 0o600 });
}

function removeDiscovery() {
  if (!discoveryFile) return;
  try { fs.unlinkSync(discoveryFile); } catch { /* already gone */ }
  discoveryFile = null;
}

function activate(context) {
  const raw = watchOutput(context);
  for (const t of vscode.window.terminals) mirrorFor(t);

  const token = crypto.randomBytes(24).toString('hex');
  const server = http.createServer((req, res) => { handle(req, res, token); });
  server.listen(0, '127.0.0.1', () => {
    writeDiscovery(server.address().port, token, raw);
  });
  server.on('error', err => console.error('mac-remote: server error', err));

  context.subscriptions.push({ dispose: () => { removeDiscovery(); server.close(); } });
  process.once('exit', removeDiscovery);
}

function deactivate() {
  removeDiscovery();
}

module.exports = { activate, deactivate };
