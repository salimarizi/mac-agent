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
- Pinch-to-zoom, momentum scroll, trackpad-style cursor control
- Accessory key bar: arrow keys, Esc, Tab, Cmd/Ctrl/Opt/Shift modifiers
- Password authentication with constant-time comparison and rate limiting
- TLS support (direct or via reverse proxy)
- TURN server support for NAT traversal
- Single HTML file client
- All credentials via environment variables — safe for public repos

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

## Environment variables

| Variable | Required | Description |
|---|---|---|
| `MAC_AGENT_PASSWORD` | Yes | Access password |
| `MAC_AGENT_TURN_URL` | No | TURN server URL, e.g. `turn:host:3478` |
| `MAC_AGENT_TURN_USER` | No | TURN username |
| `MAC_AGENT_TURN_PASS` | No | TURN credential |

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
```

## Project layout

```
mac-agent/
├── main.go                     entry point, flags, HTTP server
├── client/index.html           single-file web client
├── internal/
│   ├── signaling/signaling.go  WebSocket + auth + per-viewer lifecycle
│   ├── capture/capture.go      CGDisplayCreateImage → ffmpeg → H.264
│   ├── session/session.go      WebRTC peer: video track + data channel
│   └── input/input.go          data-channel JSON → robotgo mouse/keyboard
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

Restart Terminal after granting.

## Troubleshooting

| Problem | Fix |
|---|---|
| Black/gray screen | Grant Screen Recording, restart Terminal |
| Mouse doesn't move | Grant Accessibility permission |
| High latency | Reduce `-width` or `-bitrate`; ensure VideoToolbox is used |
| Can't connect over internet | Check SSH tunnel, nginx config, firewall 443+3478 |
| WebRTC fails through NAT | Ensure coturn is running and TURN env vars are set |

## License

MIT
