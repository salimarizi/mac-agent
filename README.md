# Mac Remote

Control your Mac from your iPhone (or any browser) over WebRTC. Low-latency screen streaming with full mouse, keyboard, zoom, and scroll support.

## How it works

```
iPhone Browser ── HTTPS ──▶ VPS (nginx + coturn)
                             ├── /    → serves web client
                             ├── /ws  → proxied to Mac via SSH tunnel
                             └── TURN → relays WebRTC media

Mac (behind any NAT):
  mac-agent ◀── SSH reverse tunnel ──▶ VPS
```

- **mac-agent** runs on your Mac: captures the screen (Core Graphics), encodes H.264 (VideoToolbox hardware encoder), handles mouse/keyboard (robotgo), manages WebRTC.
- **VPS** is the internet-facing gateway: nginx terminates TLS and serves the web client, coturn relays WebRTC media through NAT.
- **SSH reverse tunnel** connects your Mac to the VPS — no ports to open on your home network.

## Features

- H.264 hardware encoding via VideoToolbox (falls back to libx264)
- Touch mode built for phones: tap clicks where you tap, hold-and-drag drags
- Trackpad mode for pixel work, pinch-to-zoom, momentum scroll
- Fill mode so an upright phone shows a readable screen instead of a letterboxed strip
- Stream quality presets chosen from the phone (Auto / Sharp / Smooth / Saver)
- Two-way clipboard between phone and Mac
- macOS shortcut sheet (Spotlight, ⌘Tab, ⌘C/V, screenshot, …)
- Accessory key bar: arrow keys, Esc, Tab, Cmd/Ctrl/Opt/Shift modifiers
- Terminals as chats: every open Terminal.app / iTerm2 tab listed like a
  messaging app, with two-way commands that land in the real tab
- Installable to the home screen (PWA) for a chrome-less window on iOS
- Password authentication with constant-time comparison and rate limiting
- TLS support (direct or via reverse proxy)
- TURN server support for NAT traversal
- All credentials via environment variables — safe for public repos

## Using it from a phone

| Gesture | Touch mode (default) | Trackpad mode |
|---|---|---|
| Tap | click where you tapped | click at the cursor |
| Double tap | double-click | zoom in / out |
| Hold, then release | right click there | right click at the cursor |
| Hold, then drag | drag — windows, selections, files | — |
| One finger drag | scroll (pans when zoomed in) | move the cursor |
| Two finger drag | scroll | scroll |
| Two finger tap | right click | right click |
| Pinch | zoom | zoom |

Toolbar: **Keys** soft keyboard · **Touch/Pad** interaction mode · **Fit/Fill**
whole screen or filled and pannable · **Scroll** force one-finger scrolling ·
**More** shortcuts, clipboard, stats · **Full** fullscreen · **Exit** disconnect.

The chip in the status bar cycles the stream quality. *Auto* uses whatever the
agent was started with; the other presets override width, framerate and bitrate
for this viewer and restart the stream (about a second of black).

Phones start on *Smooth* rather than the agent's full-fat settings, and drop to
*Saver* when the browser reports a slow or metered link. If sustained packet loss
shows up mid-session the client steps down one preset on its own — pushing more
bitrate than the path can carry just builds a queue, and that queue is what gets
felt as lag. It never steps back up by itself; touching the chip takes manual
control and switches the automatic behaviour off for the session.

### Terminals

The menu button (top left; a permanent sidebar on wide screens) switches between
**Screen** and **Terminals**. Terminals lists every open Terminal.app and iTerm2
tab as a conversation: its latest line as the preview, the program in front
(`claude`, `zsh`, `ssh`, …), and typing dots while its output is changing. A
window holding several tabs shows as one group conversation.

Opening one shows the tab's output as incoming messages. Whatever you send is
typed into that tab on the Mac followed by Return, so it appears in the real
window immediately; output typed at the Mac shows up on the phone within a
second. **⌃C** interrupts the program in front. In a group, a tab bar under the
header switches between the window's tabs, like Terminal's own tab bar; a dot
marks tabs with unseen output, and commands go to the tab on screen.

Each output bubble has a **Copy** button, and the clipboard button pastes into
the command box. Terminal only hands out plain text, so colours are inferred:
prompts and commands, diffs, errors, warnings, links and Claude Code's markers.

It needs one more permission: System Settings → Privacy & Security →
**Automation** → mac-agent → **Terminal** (and **iTerm** if used). macOS asks on
the first use; until it is granted the Terminals screen explains how. iTerm2
exposes only the visible screen, not its scrollback.

Terminals inside VS Code-based editors (VS Code, Cursor, Antigravity) need the
small extension in `editor-extension/`. Each editor window then shows up as a
group conversation with its terminals, in their real colours:

```bash
cd editor-extension && npm install && npx @vscode/vsce package --skip-license
"/Applications/Antigravity IDE.app/Contents/Resources/app/bin/antigravity-ide" \
  --install-extension mac-remote-terminals-0.1.0.vsix   # or: code --install-extension …
```

Add `"enable-proposed-api": ["salimarizi.mac-remote-terminals"]` to the editor's
`argv.json` (`~/.vscode/argv.json`, `~/.antigravity-ide/argv.json`, …) and quit and
reopen the editor, so the extension sees everything a terminal prints. Without
it, only the output of commands started through shell integration is mirrored.
Either way, output from before the extension started is not available.
Warp, Ghostty and other terminals without a scripting interface are not listed.

**More → Stream stats** overlays resolution, framerate, bitrate, round trip,
packet loss and `path`. `path: relay` means the media is going through TURN on
the VPS instead of phone-to-Mac, which adds a round trip in each direction — the
first thing to check when it feels sluggish.

## Requirements

### Mac (runs mac-agent)

- macOS 13+
- Go 1.21+
- ffmpeg — `brew install ffmpeg`
- Xcode Command Line Tools — `xcode-select --install`
- Screen Recording + Accessibility permissions for Terminal

### VPS (gateway)

- Ubuntu 22.04+ (or any Linux with nginx, coturn, certbot)

## Quick start (LAN only)

```bash
git clone https://github.com/salimarizi/mac-agent.git
cd mac-agent

export MAC_AGENT_PASSWORD="your-secret-password"
CGO_ENABLED=1 go run . -screen 0 -fps 30 -width 1600 -addr :8443 -web client
```

Open `http://<mac-ip>:8443` on your iPhone (same Wi-Fi).

## Internet deployment

See the step-by-step guide below for full VPS setup.

### On your Mac

```bash
# Build
CGO_ENABLED=1 go build -o mac-agent .

# SSH tunnel (keeps running in background)
ssh -fNR 8443:localhost:8443 user@your-vps-ip

# Run
export MAC_AGENT_PASSWORD="your-secret-password"
export MAC_AGENT_TURN_URL="turn:remote.yourdomain.com:3478"
export MAC_AGENT_TURN_USER="turnuser"
export MAC_AGENT_TURN_PASS="turnpassword"
./mac-agent -screen 0 -fps 30 -width 1600 -addr 127.0.0.1:8443
```

### On your iPhone

Open `https://remote.yourdomain.com`, enter the password.

### Running it unattended

`scripts/install-service.sh` installs a launchd user agent that keeps the agent
and the reverse tunnel running as one supervised unit — restarting them after a
crash, a network drop, or a reboot. Fill in `.env` first (see `.env.example`).

```bash
scripts/install-service.sh          # install + start
tail -f ~/Library/Logs/MacRemote/mac-remote.log
scripts/uninstall-service.sh        # stop + remove
```

The service does **not** run from the checkout. launchd agents get no TCC grant
for `~/Documents`, `~/Desktop` or `~/Downloads`, so a checkout in any of those is
unreadable to the job and it dies with `Operation not permitted` before the
script starts. The installer stages the binary, `client/`, and `.env` into
`~/Library/Application Support/MacRemote` and points launchd there — so **re-run
the installer after changing `.env` or the Go code**.

### Shipping a client change

The client is served from two places: nginx on the VPS, and the staged copy the
agent serves locally. One script refreshes both and verifies the upload:

```bash
scripts/deploy-client.sh
```

No agent restart is needed — the files are read from disk per request. Reload the
page on the phone to pick the change up.

The gateway needs two things nginx does not do by default, or the app will not
install to a home screen and will pin itself to a stale client after a deploy:

```nginx
location = /manifest.webmanifest {
    default_type application/manifest+json;   # nginx would send octet-stream
    add_header Cache-Control "no-store" always;
}

location = /sw.js      { add_header Cache-Control "no-store" always; }
location = /index.html { add_header Cache-Control "no-store" always; }
```

## Environment variables

| Variable | Required | Description |
|---|---|---|
| `MAC_AGENT_PASSWORD` | Yes | Access password |
| `MAC_AGENT_TURN_URL` | No | TURN URLs, comma-separated — list udp/tcp/tls so a network that blocks UDP 3478 still has a fallback |
| `MAC_AGENT_TURN_USER` | No | TURN username |
| `MAC_AGENT_TURN_PASS` | No | TURN credential |
| `MAC_AGENT_STUN_URLS` | No | STUN URLs, comma-separated (defaults to Google + Cloudflare) |
| `MAC_AGENT_TLS_CERT` | No | TLS certificate for serving HTTPS directly |
| `MAC_AGENT_TLS_KEY` | No | TLS private key |

The tunnel and capture settings in `.env.example` (`MAC_AGENT_VPS_*`,
`MAC_AGENT_SSH_KEY`, `MAC_AGENT_DOMAIN`, `MAC_AGENT_FPS`, …) are read by the
supervisor script, not by the agent itself.

## Command-line flags

| Flag | Default | Description |
|---|---|---|
| `-addr` | `:8443` | Listen address |
| `-screen` | `0` | Display index (0=main, 1=second, ...) |
| `-fps` | `30` | Capture framerate |
| `-width` | `1600` | Scaled output width (0=native) |
| `-bitrate` | `6M` | Target video bitrate |
| `-ffmpeg` | `ffmpeg` | Path to ffmpeg binary |
| `-web` | | Directory of static client files to serve at / |
| `-sensitivity` | `1.6` | Trackpad move multiplier |
| `-password` | | Access password (prefer env var) |
| `-tls-cert` | | TLS certificate file |
| `-tls-key` | | TLS private key file |

## Data-channel protocol

JSON over the WebRTC data channel (see `internal/input/input.go`):

```json
{"t":"move","dx":12,"dy":-4}
{"t":"moveabs","x":0.51,"y":0.32}
{"t":"click","button":"left"}
{"t":"dblclick","button":"left"}
{"t":"down","button":"left"}
{"t":"up","button":"left"}
{"t":"scroll","dx":0,"dy":-3}
{"t":"type","text":"hello"}
{"t":"key","key":"escape","mods":["cmd"]}
{"t":"clipset","text":"…"}
{"t":"clipget"}
```

The agent answers on the same channel with `{"t":"cursor","x":…,"y":…}` after
cursor-moving events, and `{"t":"clip","text":"…"}` in reply to `clipget`.

Over the signaling socket, the viewer may also send a quality preset. Empty or
missing fields mean "keep what the agent was started with"; values are clamped
server-side, and the stream restarts only when something actually changed:

```json
{"type":"quality","payload":{"width":1280,"fps":30,"bitrate":"3M"}}
```

Terminals also travel over the signaling socket (see `internal/terminal/bridge.go`):

```json
{"type":"term_list"}
{"type":"term_watch","payload":{"ids":["/dev/ttys004","/dev/ttys008"]}}
{"type":"term_unwatch"}
{"type":"term_send","payload":{"id":"/dev/ttys004","text":"git status","nonce":"a1"}}
{"type":"term_interrupt","payload":{"id":"/dev/ttys004","nonce":"a2"}}
```

The agent answers with `term_list` (`terminals`, each with `id`, `window`,
`tab`, `title`, `process`, `preview`, `active`), `term_ack` per send/interrupt,
and `term_output` while watching: `{"id","from","lines","reset"}` means drop
every line from index `from` on, then append `lines`.

## Project layout

```
mac-agent/
├── main.go                     entry point, flags, HTTP server
├── client/                     web client (no build step)
│   ├── index.html              the whole app: markup, styles, script
│   ├── manifest.webmanifest    installable-to-home-screen metadata
│   ├── sw.js                   service worker (network-first shell cache)
│   └── icon-*.png              app icons
├── internal/
│   ├── signaling/signaling.go  WebSocket + auth + per-viewer lifecycle
│   ├── capture/capture.go      CGDisplayCreateImage → ffmpeg → H.264
│   ├── session/session.go      WebRTC peer: video track + data channel
│   ├── input/input.go          data-channel JSON → robotgo mouse/keyboard
│   └── terminal/               Terminal/iTerm tabs over Apple Events, editor terminals via the extension
├── editor-extension/           VS Code-family extension that mirrors editor terminals
├── .env.example                environment variable template
└── .gitignore
```

## Security

- Password required on every connection (constant-time compare)
- Rate limiting: 5 failed auth attempts = 30s IP block
- TURN credentials sent server-side after auth only
- WebSocket origin checking
- Security headers (X-Frame-Options, HSTS, CSP)
- All secrets via environment variables

## macOS permissions

System Settings → Privacy & Security:

1. **Screen Recording** → enable for Terminal / mac-agent binary
2. **Accessibility** → enable for the same
3. **Automation** → Terminal (and iTerm) under mac-agent, for the Terminals screen

Restart Terminal after granting.

## Troubleshooting

| Problem | Fix |
|---|---|
| Black/gray screen | Grant Screen Recording, restart Terminal |
| Mouse doesn't move | Grant Accessibility permission |
| High latency | Reduce `-width` or `-bitrate`; ensure VideoToolbox is used |
| Can't connect over internet | Check SSH tunnel, nginx config, firewall 443+3478 |
| WebRTC fails through NAT | Ensure coturn is running and TURN env vars are set |
| Terminals says "Allow terminal access" | Grant Automation → Terminal for mac-agent, restart the service |

## License

MIT
