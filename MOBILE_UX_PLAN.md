# Mobile UX Improvement Plan

Goal: make the web client at `remote.salimdev.my.id` good enough on a phone that Chrome Remote Desktop is no longer needed. The desktop experience already works; the phone experience is technically functional but impractical.

> **Status: implemented.** All three phases are built and verified locally; see *Outcome* at the end for what shipped, what was deliberately left out, and what still needs testing on a real phone.

## Context

The client (`client/index.html`, single self-contained file, no build step) already has substantial touch scaffolding: trackpad-style relative cursor movement, long-press right-click, pinch zoom (CSS transform), two-finger momentum scroll, an iOS soft-keyboard bridge (`#kbCapture` textarea with value diffing), an accessory key bar, remote cursor overlay, and reconnect supervision.

What makes it impractical on a phone today:

1. **Tap does not click where you tap.** `touchend` sends `{t:'click'}` with no preceding `moveabs` (~line 1326), so it clicks wherever the remote cursor already is. Every interaction requires trackpad-dragging the cursor onto the target first.
2. **No touch drag.** Touch never emits `down`/`up`, so dragging windows, selecting text, and drag-and-drop are impossible.
3. **No touch double-click** — double-tap is bound to zoom instead.
4. **Zoom is blurry.** Pinch zoom is a CSS transform on the already-downscaled 1600 px stream; zooming magnifies compression artifacts instead of revealing detail.
5. **Portrait letterboxing.** `object-fit: contain` renders a 16:10 Mac screen as a thin strip on a tall phone; no fit-width mode, no orientation handling. (`const screen = $('screen')` at ~line 699 shadows `window.screen`, blocking orientation APIs.)
6. **Fixed stream settings.** 6 Mbps / 1600 px / 30 fps regardless of LTE vs Wi-Fi; nothing adjustable from the phone.
7. **No conveniences:** no clipboard sync, no macOS shortcut palette, no PWA manifest (so no chrome-less standalone mode), no stats HUD.

Architecture recap (for the implementer):

- Go agent on the Mac: capture via `CGDisplayCreateImage` → ffmpeg H.264 (`internal/capture/capture.go`), WebRTC via pion v4 (`internal/session/session.go`), WS signaling with single-viewer eviction (`internal/signaling/signaling.go`), input over a data channel labeled `input` executed by robotgo on one serialized goroutine (`internal/input/input.go`, `main.go:104`).
- Input protocol (JSON on the data channel): `moveabs` (normalized 0..1 x/y), `move` (relative), `down`/`up` (button), `click`, `dblclick`, `scroll`, `key`, `type`, `backspace`. Agent pushes `{"t":"cursor",x,y}` back.
- Capture restart machinery already exists: when display geometry changes, the capture goroutine exits → `session.pump` closes → `watchSession` sends `session_ended` → client calls `restartStream()` for a fresh offer (`capture.go:281`, `session.go:202`, `signaling.go:516`, `index.html:933`). Quality switching reuses this path.
- Deploy: manual `scp client/index.html root@202.155.94.47:/var/www/mac-agent/client/index.html` for the VPS copy, plus `scripts/install-service.sh` re-run to refresh the local staged copy in `~/Library/Application Support/MacRemote`.

---

## Workstream A — Touch interaction overhaul (highest impact, client-only)

Introduce a dual interaction mode with a toolbar toggle, persisted in `localStorage` (`macagent_touch_mode`):

- **Touch mode** (new, default on touch devices): the screen behaves like a touchscreen — you interact with what you touch.
- **Trackpad mode** (current behavior, kept intact): relative cursor movement, useful for pixel-precise work.

### Touch-mode gesture state machine

All thresholds in CSS pixels; tune after device testing.

| Gesture | Detection | Emits |
|---|---|---|
| Tap | touchstart→touchend < 250 ms, movement < 10 px | `moveabs(point)` + `click left` |
| Double-tap | second tap within 300 ms and 30 px of the first | `moveabs(point)` + `dblclick` |
| Long-press, release without moving | 500 ms hold, movement < 10 px, then release | `moveabs(point)` + `click right` (+ `navigator.vibrate(30)`, reuse existing pattern ~line 1230) |
| Long-press, then move (drag) | 500 ms hold → haptic → finger moves | `moveabs(point)` + `down left`; `moveabs` on each move; `up left` on release |
| One-finger move (zoomed in) | movement ≥ 10 px before long-press fires, zoom > 1 | pan viewport (existing `clampPan`, ~line 632) |
| One-finger move (zoom = 1) | same, not zoomed | nothing (prevents accidental clicks mid-swipe) |
| Two-finger move | existing | scroll (keep momentum engine, ~lines 1144–1202) |
| Pinch | existing | CSS zoom (unchanged; see Workstream C) |
| Two-finger double-tap | new | zoom toggle 1× ↔ 2.5× (replaces one-finger double-tap zoom, which is now double-click) |

Coordinate mapping: tapped point → video coordinates must account for current CSS zoom/pan transform *and* letterboxing. Extend `videoToNorm()` (~line 1079) or add an inverse-transform helper that divides out `zoomLevel`/pan offsets before the existing letterbox math.

Implementation notes:

- Rework `touchstart`/`touchmove`/`touchend` (~lines 1218–1341) into an explicit state machine (`IDLE → PENDING_TAP → LONG_PRESS → DRAGGING / PANNING / PINCHING / SCROLLING`) with a mode branch; trackpad mode keeps the current handlers verbatim.
- In drag state, throttle `moveabs` to ~60 Hz (the agent's input channel is 256-deep and drops on overflow, `input.go:115–128`).
- Default mode on first visit: touch mode if `('ontouchstart' in window)`, trackpad on desktop.

**Risks:** long-press-drag vs long-press-right-click disambiguation (resolved by "did the finger move after the haptic"); iOS Safari synthesizing mouse events after touch (existing `preventDefault` + `{passive:false}` already suppresses this — keep it); drag interacting with the soft keyboard's blur suppression (~line 1444) — taps on the video while the keyboard is open should click, not dismiss, which the existing kbCapture refocus logic mostly handles.

## Workstream B — Quality presets (agent + client)

Let the phone pick a stream profile. Reuses the existing capture-restart cascade; no new WebRTC machinery.

Presets (client dropdown in toolbar, persisted in `localStorage`, re-sent after every `auth_ok`):

| Preset | width | fps | bitrate |
|---|---|---|---|
| Sharp (default desktop) | 1600 | 30 | 6 M |
| Balanced (default phone) | 1280 | 30 | 3 M |
| Data saver | 960 | 20 | 1.2 M |

Protocol: new authenticated WS message `{"type":"quality","width":1280,"fps":30,"bitrate":3000000}` (explicit values, so presets live client-side only).

Agent changes:

- `internal/signaling/signaling.go`: handle `quality` in the post-auth message loop — clamp values to sane bounds (width 640–native, fps 10–60, bitrate 500 k–12 M), store as the viewer's desired `capture.Config`, then stop the current capture; the existing `session_ended` → client `restartStream()` flow restarts it with the new config.
- `internal/capture` / `main.go`: the capture `Config` is currently built once from flags (`main.go:59–61`); make the per-viewer session carry its own `Config` (flag values become defaults) so restart picks up the override.
- Client: on `session_ended` triggered by a quality change, the existing handler (~line 933) already restarts the stream — no special casing needed beyond not showing a scary "session ended" status.

**Edge cases:** single-viewer eviction — a quality restart and a second viewer's `hello` racing is already handled by `adopt` (`signaling.go:148`); a brief (~1–2 s) black frame during restart is acceptable and matches the existing display-geometry-change behavior.

**Stretch (separate, later):** TWCC-driven adaptive bitrate — pion already registers the TWCC interceptor; feeding its estimate into a slow ffmpeg restart loop is possible but materially more work. Keep manual presets first.

## Workstream C — Viewport, zoom, orientation (client-only)

- **Fit-width mode for portrait:** add a fit toggle (`contain` ↔ `fit-width`). Fit-width sizes the video to 100% width and lets one-finger vertical pan move across the screen height (reuses `clampPan`). Default: fit-width in portrait, contain in landscape.
- **Fix the shadowing bug:** rename `const screen = $('screen')` (~line 699) to `screenEl` everywhere, so `window.screen.orientation` is usable.
- **Orientation handling:** on `orientationchange`/`matchMedia('(orientation: …)')`, recompute fit mode and pan clamps; attempt `screen.orientation.lock('landscape')` only in fullscreen/standalone (fails silently elsewhere).
- **Sharper zoom:** when the user pinches past ~2×, optionally bump the stream to a higher width via the Workstream B `quality` message (debounced ~2 s, only if current width < native). This makes pinch-zoom actually reveal detail instead of blur. Ship as a toggle ("HD on zoom") since each bump restarts the stream.

## Workstream D — Mobile conveniences

**Clipboard sync** (agent + client; robotgo already provides clipboard via `ReadAll`/`WriteAll`):

- Phone → Mac paste: key-bar button reads `navigator.clipboard.readText()` (user gesture + HTTPS — both satisfied), sends new data-channel message `{"t":"clipset","text":"…"}`; agent does `robotgo.WriteAll(text)` then the client follows with `key cmd+v`. Handles large text and special characters far better than replaying `type`.
- Mac → phone copy: key-bar button sends `{"t":"clipget"}`; agent replies `{"t":"clip","text":"…"}` on the data channel (same channel the `cursor` messages use, `session.go:129`); client writes it with `navigator.clipboard.writeText()` inside the button's gesture window (iOS: resolve via a tap-to-copy toast if the async reply lands outside the gesture).
- Agent: add both message types to the `input.go` switch; `clipget`'s reply needs a reference to the data channel — route it through the session rather than the input controller.

**Shortcut palette** (client-only): a bottom-sheet grid opened from the key bar: Cmd+Tab, Cmd+Space, Cmd+C/V/X/Z/A, Cmd+W, Cmd+Q, Ctrl+↑ (Mission Control), Cmd+Shift+4, Esc. Each sends existing `{t:'key'}` messages — no agent changes.

**PWA / standalone mode** (new static files): `client/manifest.webmanifest` (name, `display: standalone`, `orientation: any`, theme color matching existing `theme-color` meta), icons 192/512 px, and a minimal network-first service worker (required for Android install prompt; iOS uses the existing `apple-mobile-web-app-capable` meta + manifest). Standalone mode removes Safari's chrome — the practical answer to iOS's missing fullscreen API (~line 1536). Note: these are the first additional client files — they force Workstream E.

**Stats HUD** (client-only): small toggleable overlay fed by the existing `getStats()` watchdog loop (~line 781): decoded fps, bitrate, RTT, resolution.

## Workstream E — Deploy ergonomics

New `scripts/deploy-client.sh`:

1. `scp`/`rsync` the whole `client/` directory to `root@202.155.94.47:/var/www/mac-agent/client/` (no longer a single file once the PWA assets exist), then verify with a `shasum` compare.
2. Refresh the local staged copy in `~/Library/Application Support/MacRemote/client` (copy only — a full `install-service.sh` re-run is needed only for agent binary changes).

Shell caveats on this Mac (from prior sessions): `head` is shadowed by LWP — use `sed -n 1p`; there is no `timeout` binary.

---

## Implementation order

Each phase is independently shippable.

1. **Phase 1 — client-only interaction fixes (the big win):** Workstream A (touch mode + drag + double-click), Workstream C fit-width/orientation/shadowing fix. Deploy = one `scp` + local re-stage. No agent rebuild.
2. **Phase 2 — agent + client:** Workstream B quality presets, Workstream D clipboard sync. Requires `go build` + `install-service.sh` re-run + client deploy.
3. **Phase 3 — polish:** shortcut palette, PWA manifest/icons/service worker, stats HUD, "HD on zoom", and `scripts/deploy-client.sh` (write the script at the start of this phase, before the multi-file deploys begin).

## Verification

- **Local first:** run the agent on the Mac and open the client from a phone on the same LAN (or test touch paths in desktop Chrome DevTools device emulation) before scp'ing to the VPS.
- **Phase 1 checklist (iPhone Safari + Android Chrome):** tap clicks the tapped element; double-tap double-clicks (opens a file); long-press + release right-clicks; long-press + move drags a window and selects text; pinch still zooms; two-finger scroll still works with momentum; portrait defaults to fit-width with vertical pan; trackpad mode toggle restores old behavior exactly; soft keyboard + key bar still work after the refactor.
- **Phase 2 checklist:** switching presets visibly changes sharpness/smoothness and reconnects within ~2 s; preset survives reconnect; paste from phone lands intact (emoji, newlines, long text); copy from Mac reaches the phone clipboard on both platforms.
- **Phase 3 checklist:** Android shows the install prompt; iOS Add-to-Home-Screen launches chrome-less; shortcut palette entries fire (verify Cmd+Tab switches apps); stats HUD numbers move.
- **Regression:** desktop mouse/keyboard path untouched in trackpad mode; display sleep/wake and geometry-change restart still recover; supervisor health loop unaffected (`scripts/mac-remote-supervisor.sh`).

---

## Outcome

### Shipped

| Area | What landed |
|---|---|
| Touch mode | `client/index.html` — tap-to-click at the tapped point, double-tap double-click, hold-release right click, hold-drag drag with a held-button cursor marker, one-finger scroll (primed with a `moveabs` so it targets the window under the finger), `touchcancel` unlatching. Trackpad mode kept behind the **Touch/Pad** toggle, persisted in `localStorage`. |
| Viewport | **Fit/Fill** toggle; Fill defaults on touch devices in portrait. `transform-origin` corrected to centre (the pan and double-tap-zoom maths already assumed it), `clampPan` now clamps against the picture rather than the element, cursor overlay fixed for zoomed/panned views, `screen` renamed `screenEl` to stop shadowing `window.screen`. |
| Quality | `{"type":"quality"}` over the signaling socket; `setQuality`/`parseBitrate` in `internal/signaling/signaling.go` clamp and sanitize, per-viewer `capture.Config` on `conn`, restart only when something changed. Status-bar chip cycles Auto / Sharp / Smooth / Saver. |
| Clipboard | `clipset`/`clipget` in `internal/input/input.go`, reply channel via `SetReplyFn` in `internal/session/session.go`. Client handles Safari's gesture restriction by holding a blocked write for the next tap. |
| Shortcuts | Bottom sheet with 14 macOS chords, clipboard actions, right-click arm, stats toggle. The toolbar's old **Right** button became **More**. |
| Stats HUD | Size, fps, kbps and ping, derived from the stall watchdog's existing `getStats` poll — no extra polling. |
| PWA | `manifest.webmanifest`, `sw.js` (network-first), generated icons. The iOS `webkitEnterFullscreen` fallback was removed: it hands the picture to the native video player and swallows every touch, which is useless for a remote desktop. Add-to-Home-Screen is the real route to a chrome-less window. |
| Deploy | `scripts/deploy-client.sh` — stages locally, uploads the whole `client/` directory, verifies every file by checksum, probes `/healthz`. |

### Deliberately not built

- **HD-on-zoom** (auto-raising resolution when you pinch past 2×). Each bump restarts the capture pipeline, so it would black the picture out in the middle of the gesture that asked for it. With the quality chip now one tap away, the manual control is better behaved. Revisit only if the agent ever gains a way to change resolution without a restart.
- **Adaptive bitrate** from TWCC feedback — still the right long-term answer to a bad cellular link, still a much larger change than presets.

### Verified

- Go: `go build`, `go vet`, `gofmt`, and `internal/signaling/quality_test.go` covering bitrate parsing (including rejecting shell-looking input), clamping, auto-restore, and the no-op cases.
- Client: gesture state machine driven with synthetic `TouchEvent`s against a stubbed data channel — every gesture in the table above asserted on the exact messages emitted, in both modes; quality cycling and ordering (preset before `hello`); clipboard round trip including the blocked-write path; sheet, arm and HUD behaviour. Fill maths checked at phone dimensions (390×732 → scale 3.0, horizontal-only panning).

### Still needs a real device

Touch latency and gesture feel, iOS soft-keyboard interaction with the new handlers, Add-to-Home-Screen, and whether 500 ms is the right long-press threshold in practice.
