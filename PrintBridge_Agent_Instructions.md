# PrintBridge — Agent Build Instructions

**Product**: A standalone, independent local print/device bridge — a direct competitor to QZ Tray.
**Positioning**: Lighter install, real job persistence, cloud-push capable, modern packaging, cleaner pairing/security model.
**Stack**: Go (local agent) · TypeScript (web SDK) · Cloudflare Workers + Durable Objects (optional cloud relay tier) · SQLite (local persistence)

This document defines the full build in strict dependency order. Each module lists Objective, Tasks, Files/Structure, and Acceptance Criteria. A coding agent should implement modules **in order** — later modules assume earlier ones are complete and tested. Do not skip ahead even if a later feature looks easy; the queue, security, and transport layers are load-bearing for everything after them.

---

## Module 0 — Project Scaffolding & Repo Structure

**Objective**: Establish the monorepo skeleton so every later module has a home.

**Tasks**
- Create a monorepo with three top-level packages:
  - `/agent` — Go local agent (the core product)
  - `/sdk` — TypeScript client SDK for web apps (`printbridge.js` equivalent to `qz-tray.js`)
  - `/relay` — Cloudflare Worker + Durable Object cloud relay (optional tier, built later)
- Set up Go modules (`go.mod`) in `/agent`, targeting Go 1.22+.
- Set up `/sdk` as a standalone npm package (buildable with `tsup` or `esbuild`, output both ESM and UMD for `<script>` tag use).
- Add root `README.md`, `LICENSE` (decide MIT for SDK, and your own license for `/agent` if going open-core), and a `docs/` folder for architecture notes.
- Add CI skeleton (GitHub Actions) with placeholder jobs for `agent-build`, `agent-test`, `sdk-build`, `sdk-test` — fill in real steps as each module lands.

**Acceptance Criteria**
- `go build ./...` succeeds in `/agent` with a stub `main.go` that prints a version string.
- `npm run build` succeeds in `/sdk` with a stub export.
- Repo has clear top-level structure with no ambiguity about where new code goes.

---

## Module 1 — Printer Discovery & Raw Passthrough Printing (Core MVP)

**Objective**: Match QZ Tray's single most important feature — enumerate system printers and send raw bytes (ESC/POS, ZPL, EPL) directly to a printer driver, bypassing OS print dialogs. Without this, nothing else matters.

**Tasks**
- Implement OS-specific printer enumeration:
  - Windows: use `winspool.drv` bindings (via `golang.org/x/sys/windows` or a cgo wrapper) to list installed printers.
  - macOS/Linux: shell out to CUPS (`lpstat -p` / `lpr` raw mode, or bind to `libcups` directly for a cleaner integration).
- Implement a `PrintRaw(printerName string, data []byte) error` function per OS that sends raw bytes bypassing the print spooler's rendering (i.e., true passthrough, not "print this as a text file").
- Implement `ListPrinters() ([]PrinterInfo, error)` returning name, driver, default-flag, and online/offline status where the OS exposes it.
- Write a CLI test harness (`agent/cmd/printtest`) that lists printers and sends a raw ESC/POS test string (`"Hello PrintBridge\n\n\n" + cutCommand`) to a chosen printer by name, for manual verification against a real thermal printer.
- Handle codepage/encoding concerns explicitly — accept raw `[]byte` only, no string coercion, so callers control encoding (this matters a lot given multi-language text — leave encoding to the caller, don't silently mangle it).

**Acceptance Criteria**
- Running the CLI harness against a real USB/network thermal printer produces a physical printout with no OS print dialog appearing.
- Printer list correctly reflects installed printers on Windows, macOS, and Linux (test on at least two of the three).
- Raw byte sequences pass through unmodified (verify with a hex dump comparison before/after send).

---

## Module 2 — Job Queue & Persistence

**Objective**: This is the single biggest reliability upgrade over QZ Tray — no job should ever silently vanish because a printer was offline or jammed.

**Tasks**
- Embed SQLite (via `mattn/go-sqlite3` or pure-Go `modernc.org/sqlite` to avoid cgo) as the local persistence layer, stored in the OS-appropriate app-data directory.
- Schema: `jobs` table with `id, printer_name, payload (blob), status (queued/sending/success/failed/retrying), created_at, last_attempt_at, attempts, error_message`.
- Implement a job queue worker (goroutine + channel-based) that:
  - Picks up `queued` jobs and attempts delivery via Module 1's `PrintRaw`.
  - On failure, applies exponential backoff and increments `attempts`, capping at a configurable max (default 5) before marking `failed`.
  - On success, marks `success` and records timestamp.
  - Survives agent restart — on boot, requeue any jobs left in `sending`/`retrying` state.
- Expose an internal Go API: `Enqueue(printerName string, payload []byte) (jobID string, err error)`, `GetJobStatus(jobID string) (JobStatus, error)`, `ListRecentJobs(limit int) ([]Job, error)`.
- Add a retention/cleanup policy (e.g., auto-delete `success` jobs older than 7 days, keep `failed` longer for diagnostics).

**Acceptance Criteria**
- Killing the agent process mid-job and restarting it results in the job either completing or correctly reflecting a retry, never silently disappearing.
- Unplugging a printer, enqueueing 5 jobs, then reconnecting the printer results in all 5 eventually printing in order.
- `ListRecentJobs` returns accurate status for a mixed batch of successful/failed/retrying jobs.

---

## Module 3 — Local WebSocket Server (Browser-Facing API)

**Objective**: Provide the same integration point QZ Tray offers — a local WebSocket server a web page can connect to — but backed by the persistent queue instead of firing print jobs directly.

**Tasks**
- Start a local WebSocket server on a configurable port (default `ws://localhost:9567`, avoid QZ's default ports to prevent conflicts on machines that might run both during migration).
- Define a JSON message protocol, versioned from day one (`{"version": 1, "type": "...", "payload": {...}}`), covering:
  - `list_printers` → returns Module 1's printer list
  - `print` → accepts `{printer, data (base64), format}` → calls Module 2's `Enqueue`, returns `{jobId}` immediately (non-blocking, unlike QZ's typical synchronous feel)
  - `job_status` → query a job by ID
  - `subscribe_job_updates` → client can subscribe to push updates on a job (success/fail) via the same socket
- Implement basic origin checking (`Origin` header allowlist, configurable) as a first security layer — refined further in Module 5.
- Handle multiple simultaneous browser connections (e.g., admin panel + POS page both connected at once) without cross-talk.

**Acceptance Criteria**
- A minimal HTML test page can connect via `new WebSocket("ws://localhost:9567")`, call `list_printers`, then `print`, and receive a `jobId` immediately, followed by an async status push once the job completes.
- Two browser tabs connected simultaneously both receive correct, independent responses.
- Malformed or unversioned messages are rejected with a clear error, not a crash.

---

## Module 4 — System Tray Application & Lifecycle

**Objective**: Give the agent a real presence on the user's machine — this is what non-technical staff will actually see and trust.

**Tasks**
- Integrate `getlantern/systray` (or `fyne.io/systray`) for a cross-platform tray icon.
- Tray menu items: `Status: Connected/Offline`, `Open Dashboard` (opens local web UI, built in Module 10), `View Recent Jobs`, `Restart Agent`, `Quit`.
- Implement auto-launch on OS boot/login (Windows: registry Run key or Task Scheduler; macOS: LaunchAgent plist; Linux: `.desktop` autostart entry).
- Implement graceful shutdown — on quit, stop accepting new WebSocket connections, let in-flight jobs finish or persist to queue, then exit.
- Tray icon should visually reflect state (e.g., icon badge or color change) for: all-good / job failed / printer offline — a passive glance-value QZ Tray doesn't offer.

**Acceptance Criteria**
- Agent appears in system tray on all three OSes with working menu items.
- Rebooting the machine results in the agent auto-starting with no user action.
- Tray icon visibly changes state when a print job fails or a known printer goes offline.

---

## Module 5 — Security & Pairing Model

**Objective**: Replace QZ's clunky cert-signing ceremony with something a non-technical user can actually complete, while still preventing arbitrary websites from silently printing.

**Tasks**
- On first launch, agent generates a random pairing code (6-digit, or a short human-friendly string) displayed in the tray UI/dashboard.
- A web app wanting to use the agent must complete a **pairing handshake**:
  1. Web app connects to the WebSocket server and sends `request_pairing`.
  2. Agent shows the pairing code in its UI; user enters it into the web app (out-of-band channel = human eyes, not the network).
  3. Web app sends `confirm_pairing` with the code; agent verifies and issues a long-lived signed token (HMAC or Ed25519-signed) stored by the web app for future connections.
  4. Subsequent connections present the token in the initial handshake message; agent validates and skips re-pairing.
- Maintain a local allowlist of paired origins/tokens, viewable and revocable from the tray dashboard ("Paired apps: [domain] — Revoke").
- Rate-limit pairing attempts to prevent brute-forcing the pairing code.
- Document this flow clearly for developers — this replaces QZ's "buy/generate an RSA cert" step entirely for the common case, while an advanced cert-based mode can be added later for enterprise/white-label needs (stub this as a future module, don't build yet).

**Acceptance Criteria**
- An unpaired web app attempting to call `print` is rejected with a clear "pairing required" error.
- Completing the pairing flow end-to-end (code shown → entered → token issued) results in a working, persistent connection across agent restarts.
- Revoking a paired app from the tray dashboard immediately invalidates its token on next connection attempt.

---

## Module 6 — Printer Health Telemetry

**Objective**: Give both the tray UI and any connected dashboard real visibility into device state — something QZ doesn't expose at all.

**Tasks**
- Implement periodic polling (configurable interval, default 30s) of paired/known printers: online/offline reachability check, and where the printer/driver exposes it, paper-out/cover-open status via ESC/POS status bytes (`DLE EOT` real-time status commands) for receipt printers.
- Push status changes to connected WebSocket clients via a `printer_status_changed` event (no polling required client-side).
- Store last-known-status in the SQLite DB so the dashboard can show "last seen" even if the agent restarts.
- Expose a `get_printer_health` message type returning current status for all known printers.

**Acceptance Criteria**
- Physically disconnecting a printer results in a status change event within one polling interval, visible in both the tray icon and any connected web client.
- Triggering a paper-out condition on a supporting thermal printer is correctly reflected in status (test against real hardware; some printers/drivers won't support this — degrade gracefully to "reachable/unreachable" only).

---

## Module 7 — USB/HID & Serial Device Support (Cash Drawers, Scanners, Scales)

**Objective**: Match QZ's non-printer hardware I/O — cash drawer kick, barcode scanner input, serial scale reads.

**Tasks**
- Cash drawer: implement `OpenDrawer(printerName string) error` as a first-class API — this is typically done by sending a specific byte sequence through the *printer's* port (most drawers are RJ11-wired through the receipt printer), so this can largely reuse Module 1's raw print path with a documented drawer-kick command table for common printer brands.
- Serial: integrate `go.bug.st/serial` for direct serial port read/write (scales, some older scanners), exposed via WebSocket messages `serial_open`, `serial_write`, `serial_read`/`serial_subscribe`.
- USB/HID: integrate a Go HID library (e.g. `sstallion/go-hid`) for direct HID device communication (barcode scanners that don't act as keyboard-emulation, custom card readers).
- Add device discovery messages (`list_serial_ports`, `list_hid_devices`) mirroring Module 1's printer discovery pattern for consistency.

**Acceptance Criteria**
- `OpenDrawer` physically triggers a connected cash drawer via a receipt printer's RJ11 port on real hardware.
- A connected serial scale's weight reading is received correctly via `serial_subscribe`.
- HID device list correctly enumerates a connected barcode scanner or card reader.

---

## Module 8 — Web SDK (`printbridge.js`)

**Objective**: Provide the developer-facing library, mirroring QZ's `qz-tray.js` ergonomics closely enough that migration is easy, while exposing the new capabilities (queue status, health events, pairing).

**Tasks**
- TypeScript package with a promise-based API:
  ```
  printbridge.connect()
  printbridge.pairing.start() / printbridge.pairing.confirm(code)
  printbridge.printers.find()
  printbridge.printers.health()
  printbridge.print(printerName, data, { format }) → returns { jobId, promise-that-resolves-on-completion }
  printbridge.jobs.status(jobId)
  printbridge.devices.openDrawer(printerName)
  printbridge.on('printer_status_changed', handler)
  printbridge.on('job_status_changed', handler)
  ```
- Handle reconnection transparently (auto-retry WebSocket connection with backoff if the local agent restarts).
- Ship both an ESM build (for bundlers) and a plain `<script>` UMD build (for drop-in use, matching QZ's most common integration pattern).
- Write integration examples: vanilla HTML/JS, and a React hook example.

**Acceptance Criteria**
- A fresh web project can `npm install` the SDK, call `connect()`, pair, and print a raw ESC/POS payload with under 10 lines of code.
- Reconnection works transparently after simulating an agent restart mid-session.

---

## Module 9 — Cloud Relay (Optional Tier, Cloudflare Workers + Durable Objects)

**Objective**: The core architectural advantage over QZ Tray — allow a backend server (not just a live browser tab) to push a print job to a remote agent.

**Tasks**
- Local agent gains an optional outbound mode: maintains a persistent authenticated WebSocket connection to a cloud relay endpoint (opt-in, off by default for privacy/simplicity).
- Cloud relay implemented as a Cloudflare Durable Object per paired agent/account, holding the live connection and a lightweight job-forwarding queue.
- Expose a simple REST API on the relay (`POST /v1/agents/{agentId}/print`) that authenticated backend services can call; relay forwards to the connected agent over its persistent socket, and the agent's own Module 2 queue takes over from there (so reliability guarantees don't regress even through the relay hop).
- Handle the "agent offline" case at the relay layer: queue the job at the relay level too (short-term, e.g. 24h TTL) and deliver on reconnect, or return a clear `agent_offline` error depending on configured mode.
- Auth: API keys per registered agent/account, issued via a simple relay-side registration flow.

**Acceptance Criteria**
- A backend server with no browser involved can `POST` a print job to the relay and have it physically print on a machine on a different network, with the local agent's queue/retry guarantees still applying.
- Taking the agent offline, sending a job, then bringing it back online results in correct delayed delivery (or a clear offline error, per configured mode).

---

## Module 10 — Local Dashboard UI

**Objective**: A modern, in-browser local config/status UI — QZ Tray has effectively nothing here.

**Tasks**
- Serve a small local web UI from the agent itself (e.g., `http://localhost:9568`), opened via the tray menu's "Open Dashboard" item.
- Screens: printer list + health status, recent job history with filter (success/failed/retrying), paired apps list with revoke action, pairing code display, basic settings (port config, polling interval, auto-launch toggle, cloud relay opt-in toggle).
- Build with a lightweight stack (plain HTML/CSS/vanilla JS or a small Preact bundle) to keep the agent binary size down — avoid a heavy SPA framework here.

**Acceptance Criteria**
- Dashboard accurately reflects live state (printers, jobs, pairings) without requiring a manual refresh (poll or reuse the WebSocket for live updates).
- All settings changes take effect without requiring a full agent reinstall.

---

## Module 11 — Auto-Update Mechanism

**Objective**: Replace QZ's manual-update UX with silent, safe self-updating.

**Tasks**
- Implement update checking against a release manifest (hosted JSON: `{version, url, sha256}` per platform).
- Use a self-update library pattern (e.g. `minio/selfupdate`-style binary replace) with signature/checksum verification before applying.
- Update check runs on a schedule (e.g. daily) and respects "don't update while jobs are in-flight" — wait for queue to drain before swapping the binary.
- Provide a manual "Check for updates" tray/dashboard action, and a rollback-to-previous-version safety net (keep last-known-good binary).

**Acceptance Criteria**
- A test release bump is detected, downloaded, checksum-verified, and applied with the agent restarting cleanly and resuming queued jobs.
- A corrupted/tampered update package is rejected and does not get applied.

---

## Module 12 — Receipt/Label Templating Helpers (Differentiator Layer)

**Objective**: Go beyond QZ's "you write raw ESC/POS yourself" model — ship a templating convenience layer in the SDK, without forcing anyone to use it (raw passthrough from Module 1 always remains available).

**Tasks**
- In the TypeScript SDK, add an optional builder API:
  ```
  const receipt = printbridge.receipt()
    .logo(imageBytes)
    .text("Order #1234", { align: 'center', bold: true })
    .qr("https://.../track/1234")
    .divider()
    .lineItems([...])
    .cut();
  printbridge.print(printerName, receipt.toESCPOS());
  ```
- Support common codepage selection explicitly (e.g. CP852/CP1252/UTF-8-with-fallback) so multi-language text (including diacritics) renders correctly instead of silently corrupting — expose this as a required, explicit parameter rather than a hidden default.
- Support a ZPL label builder equivalent for label printers, at a basic level (text fields, barcode, QR).
- Keep this layer fully optional and separate from the transport/queue core — it should compile out of the agent binary entirely (it's SDK/client-side only), keeping the agent lean.

**Acceptance Criteria**
- Building a receipt via the builder API and printing it produces correctly formatted, correctly encoded output on real thermal printer hardware, including non-ASCII characters.
- Switching codepage parameter changes rendering as expected on hardware that supports multiple codepages.

---

## Module 13 — Packaging & Installers

**Objective**: Ship this in a way that feels like a modern product, not a dev tool.

**Tasks**
- Cross-compile the Go agent for Windows (amd64/arm64), macOS (amd64/arm64, notarized), Linux (amd64/arm64, deb + AppImage).
- Build native installers: `.msi` or Inno Setup for Windows, `.pkg` for macOS (with notarization/signing — required or Gatekeeper blocks it), `.deb`/AppImage for Linux.
- Installer should register auto-launch (Module 4) and drop the tray binary in the right OS-specific location automatically — zero manual config for the end user.
- Code-sign all binaries (Windows Authenticode cert, Apple Developer ID) — unsigned binaries will trigger scary OS warnings that undermine the "better than QZ" pitch immediately.

**Acceptance Criteria**
- A non-technical tester can download the installer for their OS, run it, and have the agent running in the tray with zero terminal/command-line interaction.
- No OS security warnings appear on a clean install (signing verified).

---

## Module 14 — Testing & QA Harness

**Objective**: Ensure reliability claims (the core marketing differentiator vs. QZ) are actually true, continuously.

**Tasks**
- Unit tests for queue retry/backoff logic (Module 2) using a mock printer backend that can simulate failures on demand.
- Integration test suite that runs the full agent against a mock/virtual printer (e.g. a raw TCP listener simulating a network thermal printer) for CI, since real hardware isn't available in CI.
- Manual hardware test checklist (documented, not automated) covering: real USB thermal printer, real network thermal printer, real cash drawer, real barcode scanner — to run before each release.
- Load test: enqueue several hundred jobs rapidly, verify ordering and no job loss.

**Acceptance Criteria**
- CI runs the full mock-based integration suite on every commit.
- Documented manual hardware checklist exists and is run before tagging a release.

---

## Module 15 — Documentation & Developer Onboarding

**Objective**: Make adoption easy for third-party developers — this is a product, not an internal tool.

**Tasks**
- Public docs site (can be a simple static site or docs framework) covering: install guide, pairing flow, SDK API reference, migration guide from QZ Tray (map QZ's API calls to PrintBridge equivalents 1:1 where possible), troubleshooting.
- Explicit "Migrating from QZ Tray" page — this is a strong adoption lever, since the main early audience is people currently frustrated with QZ.

**Acceptance Criteria**
- A developer unfamiliar with the project can go from zero to a working print call using only the docs, in under 15 minutes.

---

## Build Order Summary

```
0. Scaffolding
1. Raw printer discovery + passthrough print   ← core parity with QZ
2. Job queue & persistence                     ← core differentiator
3. Local WebSocket server
4. System tray app
5. Security & pairing                          ← replaces QZ's cert pain
6. Printer health telemetry                     ← differentiator
7. USB/HID/Serial device support
8. Web SDK
9. Cloud relay                                  ← biggest differentiator
10. Local dashboard UI                          ← differentiator
11. Auto-update                                 ← differentiator
12. Receipt templating helpers                  ← differentiator
13. Packaging & installers
14. Testing & QA harness
15. Docs & onboarding
```

Modules 0–5 constitute a genuinely usable MVP that already matches QZ Tray's core function with better reliability (queue) and a friendlier security model. Modules 6–12 are what make it a *better* product rather than a clone. Modules 13–15 are what make it shippable to strangers.

## Notes on Naming & IP

Avoid using "QZ" in the product name or marketing copy beyond factual comparison statements (e.g., "migrate from QZ Tray" is fine as a factual descriptor; don't imply affiliation or endorsement). Do not copy QZ Tray's source code, icons, or documentation text verbatim — this spec describes *feature parity and improvements*, not code reuse. Building an independent implementation of a similar feature set (raw printing, WebSocket bridge, etc.) is standard practice and not itself an IP issue; copying their actual code or branding would be.
