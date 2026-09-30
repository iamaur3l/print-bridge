# PrintBridge — Update & Run Plan

Status: **P0 (security + browser permissions) is implemented and tested.** Everything
else below is planned work, ordered by risk.

Reference implementation for the Windows domain problems: **XP Thermal Service**
(`github.com/xenithpulse/xp-thermal-service`, MIT) — a Windows-only Node service.
It is used here as the yardstick for what "survives a real restaurant" means, not
as code to copy (see §7).

---

## 0. Scorecard: where PrintBridge stands

### Structural advantages (keep and market these)

| Advantage | Why it matters vs. the reference implementation |
|---|---|
| Single static Go binary, no runtime | XP requires Node 18+ installed on **every** till plus an `npm install`/`tsc` build |
| Direct `winspool` binding (no shelling out) | XP pays "one PowerShell process per snapshot, not 2N" and needs four enumeration fallbacks because WMI/CIM is fragile |
| Crash-safe SQLite/WAL queue with stuck-job recovery | XP uses in-memory `sql.js` and relies on flushing "on every exit path" |
| Browser WebSocket protocol (multiplexed, correlated ids, push events) | XP is request/response HTTP per call plus SSE, with CORS + API-key handling per origin |
| Cross-platform (CUPS path, launchd/systemd/POSIX autostart) | XP is Windows 8+ only |
| Tray app, embedded dashboard, cloud relay, auto-update | XP has none of these (and deliberately does not self-update) |

### Gaps, ranked by impact

1. **Windows hardware reality.** Naive `WorkOffline` handling (a *live* bug: a stuck
   flag reports a working printer as offline), no USB port-migration following, no
   driver-rename rebinding, **no TCP 9100 network-printer path**, no printer roles,
   no corroborated health verdict, no shared cached snapshot.
2. **Operational productisation.** No service install (SCM recovery / Spooler
   dependency / single-instance lock), no config file, no port-conflict strategy,
   no `/health` verdict, no dead-letter / pause / priority / idempotency, no
   handover documentation, no alerting.
3. **Browser permission reality (new, and it affects the core promise).**
4. **Security was claimed but not implemented** — see §1 for what has now been fixed.
5. **Nothing is shippable yet:** CI does not run on `master`, release jobs
   cross-compile cgo-dependent targets from Linux, referenced docs are missing, the
   updater points at a domain we do not own, and the Go module path does not match
   the repository.

### The browser constraint that changes the framing

| Release | Change |
|---|---|
| Chrome 138 | `chrome://flags/#local-network-access-check` for testing |
| **Chrome 142 (Oct 2025)** | Local Network Access permission prompt ships for fetch/XHR to local addresses |
| **Chrome 147 (Apr 2026)** | LNA extended to **WebSocket** connections (WebTransport/WebRTC planned) |

Consequences:

- A POS UI on a **public HTTPS origin** connecting to `ws://localhost:9567` now
  triggers a permission prompt; a denial is indistinguishable from "agent not
  installed" unless the app classifies the failure.
- **Exempt:** Firefox and Safari; and pages served from `localhost`, `127.0.0.1`
  or a private IP contacting the same address space.
- Two permissions exist (`local-network`, `loopback-network`) aliased as
  `local-network-access`; iframes need `allow="local-network-access"`.
- Enterprise fleets can pre-grant via the `LocalNetworkAccessAllowedForUrls` policy.
- Strategic read: the browser bridge is now permission-gated, so the **cloud relay
  (backend → relay → agent → printer, no browser involved) becomes the durable
  path** for SaaS POS deployments — which is why the relay hardening in §4 is no
  longer optional.

---

## 1. P0 — Security & browser permissions ✅ implemented

### What changed

| # | Change | Files |
|---|---|---|
| 1 | **The pairing code is no longer returned over the WebSocket.** `request_pairing` answers `{code_requested, expires_at}`; the code is displayed only in the tray (`auth.ActivePairingCode`) and the dashboard (`/api/dashboard/summary → pairing_code`). Previously any page could self-pair by reading the code out of its own response. | `agent/pkg/server/server.go`, `README.md`, `docs/architecture.md`, `docs/sdk_guide.md`, `sdk/src/index.ts` |
| 2 | **Auth gate on every privileged message.** Only `ping`, `request_pairing`, `confirm_pairing`, `authenticate` are allowed pre-auth; everything else returns `pairing_required` and is written to the audit log. `open_drawer`, `list_printers`, `get_printer_health`, `job_status`, `list_serial_ports`, `list_hid_devices`, `check_update` and `apply_update` were previously unauthenticated. | `agent/pkg/server/server.go` (`preAuthMessages`, `dispatch`) |
| 3 | **Origin allowlist** replaces `CheckOrigin: return true`. Loopback origins are trusted by default; anything else needs `-allow-origin` (exact origin or `scheme://host:*`). Disallowed origins get HTTP 403 before the upgrade. | `agent/pkg/server/server.go`, `agent/main.go` |
| 4 | **Token origin binding.** `ValidateToken` rejects a token presented from an origin other than the one it was issued to, and audits the rejection. | `agent/pkg/auth/auth.go` |
| 5 | **Push events gated.** `job_status_changed` / `printer_status_changed` are only pushed to authenticated clients (the auth flag became `atomic.Bool` so subscriber goroutines read it safely). | `agent/pkg/server/server.go` |
| 6 | **Client IP attribution** uses `RemoteAddr` by default; `X-Forwarded-For` is trusted only with `-trust-proxy`. Audit rows now carry the real IP. | `agent/pkg/server/server.go`, `agent/main.go` |
| 7 | **SDK: browser-permission awareness and error classification.** Added `PrintBridgeError` + `code` (`not_connected`, `timeout`, `pairing_required`, `agent_absent`, `request_failed`), `permissions.localNetwork()`, `diagnose()`, `wss://` via `secure` (defaults true on HTTPS pages), per-request timeout, pending requests rejected on close, exponential reconnect backoff, and a `disconnect()` that stays disconnected. | `sdk/src/index.ts` |
| 8 | `go vet ./...` is clean again (fixed `append` with no values in the print harness). | `agent/cmd/printtest/main.go` |

### Tests added

- `agent/pkg/server/server_test.go`: `TestPairingCodeIsNotReturnedOverWebSocket`,
  `TestPrivilegedMessagesRequireAuthentication` (9 message types plus "no print side
  effect"), `TestPingWorksBeforePairing`, `TestDisallowedOriginIsRejected`,
  `TestAllowedOriginsOption`.
- `agent/pkg/auth/auth_test.go`: `TestTokenIsBoundToItsOrigin`.
- The existing `TestWebSocketServerLifecycle` now reads the pairing code the way a
  human would (from the tray/dashboard).

### Still to do in P0

1. **Token expiry + persisted HMAC secret.** Tokens never expire, and the HMAC
   secret is regenerated per process (validation is a DB lookup, so the signature is
   currently decorative). Add `expires_at` to `paired_apps`, an
   `agent_meta(key, value)` table for the secret, and refresh-on-use.
2. **Dashboard API authentication.** `/api/dashboard/revoke` and
   `/api/dashboard/pairing-code` are unauthenticated on loopback. Add Host
   validation (DNS-rebinding defence) plus a per-install dashboard token injected
   into the page at serve time.
3. **Relay authentication** — see §4: the relay currently accepts anyone as any
   agent.

---

## 2. P1 — Windows reliability (the domain bar)

### ✅ Implemented (items 1–3 and 5)

| Item | What landed | Files |
|---|---|---|
| **1. Corroborated printer status** | The naive `WorkOffline OR PRINTER_STATUS_OFFLINE` check is gone. `EvaluateStatus` applies corroboration in priority order: hardware fault → offline; a raw write inside a 2-minute trust window → online (overrides the offline flags); paused queue or absent port → offline; offline flags alone → **`unknown`** with an explanation; unrecognised status codes → ready. Real faults outrank a successful write because `WritePrinter` succeeds into the queue even when the device is out of paper. | `agent/pkg/printer/health.go`, `agent/pkg/printer/printer_windows.go`, `agent/pkg/printer/printer_posix.go`, `agent/pkg/printer/printer.go` |
| **2. Shared cached snapshot** | `Snapshotter` (4 s TTL, single-flight, copy-on-read) now serves telemetry, the dashboard, `list_printers` and `/health`, so N consumers no longer cost N enumerations. | `agent/pkg/printer/snapshot.go` |
| **3. `GET /health` verdict** | `healthy`/`degraded`/`unhealthy` + plain-language `reasons[]` + queue counters + `service` identity, 503 when unhealthy, foreign `Host` headers rejected (DNS-rebinding defence), no CORS. | `agent/pkg/health/health.go`, `agent/pkg/server/server.go`, `agent/pkg/queue/store.go` (`JobCounts`, `CountJobsByStatus`) |

Supporting changes: `state` / `status_detail` / `stale_work_offline` now flow through
`PrinterInfo` → `PrinterHealth` → SQLite (`printer_telemetry` gained two columns via
an idempotent additive migration) → the SDK types. The tray and dashboard read
`IsOnline` as before.

Tests: `agent/pkg/printer/health_test.go` (stale-flag regression, every fault class,
unknown-codes-are-ready, write trust window and expiry, write tracker, snapshot
caching, no cache aliasing, single-flight), `agent/pkg/health/health_test.go`
(unhealthy/degraded/healthy, stale-flag explanation, failed jobs, report fields,
enumeration failure), `agent/pkg/server/server_test.go` (`TestHealthEndpoint`,
`TestHealthRejectsForeignHostHeader`).

### Remaining in P1

4. **Follow the cable — ✅ detection implemented, repair deferred.**
   The live device → port map is read from
   `HKLM\SYSTEM\CurrentControlSet\Enum\<bus>\<hardware id>\<instance>\Device Parameters\PortName`
   (`agent/pkg/printer/ports_windows.go`, POSIX stub alongside), each queue's
   last-known-good port + stable device identity is persisted in `printer_bindings`,
   and `PlanPortBindings` (`agent/pkg/printer/resolver.go`) decides `none` /
   `rebind` / `ambiguous` / `missing` — never taking a port another queue owns, and
   refusing to guess between identical devices on one hub. Surfaced as the
   auth-gated `diagnose_printers` RPC and logged at startup
   (`[Server] Port check: N device-backed queue(s) tracked, M live device mapping(s)`).
   Verified live on Windows: the registry walk runs clean.
   **Still to do:**
   - *Applying* a `rebind` to the spooler, and *clearing* a stale `WorkOffline` flag.
     Both need a `GetPrinter`/`SetPrinter` level-2 round trip (the `Attributes` and
     `PortName` fields live inside a self-referential block) and must be validated on
     real hardware before they are allowed to mutate a customer's spooler. The
     resolver already computes the target port, so this is a narrow change once a
     test machine with two USB thermal printers (including two identical units) is
     available.
   - Rebinding after a driver reinstall changes the queue name to `(Copy 1)`: the
     detection path is in place for *ports*; name matching is the remaining half.
   - Validating the port map against a machine that actually has USB printers
     (this development machine has none, so the live run reported
     `0 live device mapping(s)` — correct, but not yet a positive case).
5. **TCP 9100 network adapter — ✅ implemented.**
   Queues whose spooler port is a TCP address (`IP_192.168.1.100`, `10.0.0.5:9100`,
   `printer.local`) are now written to directly over their RAW/JetDirect port, chosen
   per job by `Router` (`agent/pkg/printer/network.go`); everything else keeps the
   spooler path unchanged. Details:
   - `ParseNetworkPort` recognises `IP_`-prefixed ports (including Windows' `_1`
     duplicate suffix), bare addresses and explicit ports, and deliberately refuses
     `USB`/`LPT`/`COM`/`DOT4`/`WSD`/`PORTPROMPT:`/`SHRFAX:`/`CUPS`/`URL_`/`TCPMON`
     so existing installs cannot change behaviour.
   - A network job falls back to the spooler only when the connection was never
     established. After a partial write it returns `PartialWriteError` instead, so the
     queue retries rather than printing the receipt twice.
   - Health gains a third corroboration source: a TCP probe. A printer that answers is
     `online` ("Ready (transport reachable)") even when the spooler says otherwise; one
     that refuses is `offline` ("Not reachable"). Probes run in parallel with a 600 ms
     deadline inside the cached snapshot refresh, and hardware faults still outrank a
     successful probe.
   - `PrinterInfo` now carries `type` (`local` | `network`) and `network_address`, which
     flows through telemetry, `/health` and the SDK types.
   - Tests: 16-case port-parsing table, a real end-to-end socket delivery test
     (bytes received by a local listener + write recorded), connection-refused
     (asserting it is *not* a partial write), probe up/down, per-queue transport
     routing including case-insensitive matching and enumeration failure, and the
     reachability verdict branches.
   **Remaining:** no positive-case validation against a real Ethernet printer (none is
   available here), and the direct path currently applies to all queues whose port
   is a TCP address — a per-queue opt-out may be worth adding with the config file in
   P2.
6. **Printer roles — ✅ implemented.**
   Roles are stored in `printer_roles` (`agent/pkg/roles`), addressed as
   `payload.role` on `print`, `open_drawer`, `test_print` and `diagnose_printer`, and
   managed with `list_printer_roles` / `assign_printer_role` /
   `remove_printer_role`. Role names are normalised, resolution is case-insensitive
   against the live printer list, reassignment preserves `created_at`, and an
   unresolved role reports a reason instead of failing silently. Capability profiles
   (`agent/pkg/printer/escpos.go`) drive the built-in receipt/kitchen/text test slips;
   the builder only emits bold and cut sequences the profile claims. `diagnose_printer`
   composes transport, port binding, roles, spooler verdict and last successful write
   into a plain-language `checks` list. The SDK gained `roles.*`, `printToRole`,
   `testPrint` and `diagnosePrinter`.
   **Remaining:** no dashboard UI for role assignment yet (the WS API is complete),
   and queued jobs keep the device they were addressed to rather than following a
   later reassignment.

### Original specification (items 1–3)

```text
1. Corroborated printer status — stop treating PRINTER_ATTRIBUTE_WORK_OFFLINE alone as
   offline; require a real fault bit or physical absence; treat unknown status codes as
   ready; treat a completed WritePrinter as ground truth.
2. Shared cached snapshot with single-flight (~4s TTL).
3. GET /health verdict — deliberately not a liveness check.
```

## 3. P2 — Operational productisation

- **Queue — ✅ implemented.** `jobs` gained `priority`, `idempotency_key` and
  `next_attempt_at` (additive migrations plus a partial unique index on the key).
  `GetPendingJobs` orders by `priority DESC, created_at ASC` and only returns jobs
  whose retry time has passed. Retries are written to the database by
  `ScheduleRetry` instead of `time.Sleep`-ing inside the worker loop, and dispatch
  now runs one job per printer in parallel (`claimPrinter`/`releasePrinter`, capped by
  `WithMaxConcurrentJobs`) so a hung or jammed printer cannot delay another station —
  the regression test for that is `TestQueueHungPrinterDoesNotBlockOthers`. Jobs that
  exhaust their attempts land in the dead-letter state (`failed`), which `/health`
  reports by name; `retry_job` and `clear_failed_jobs` are the operator actions, and
  `pause_queue`/`resume_queue` hold dispatch without dropping work. New work is
  refused past `maxDepth` (5000) with `ErrQueueFull`, and a completed job wakes the
  dispatcher so a station's next receipt starts immediately rather than on the next
  tick. Completing this item also fixed a **latent flakiness bug**: `:memory:`
  databases now pin the pool to one connection (`no such table: jobs`).
  **Remaining in P2:** port fallback, the docs suite, and dashboard UI for the queue
  and role actions.
- **Windows service — ✅ implemented** (`agent/pkg/service`, built on
  `x/sys/windows/svc` and `svc/mgr`): `-service install|uninstall|start|stop|status|repair`,
  auto-start, `LocalSystem`, a declared print-spooler dependency, SCM recovery actions
  of restart-after-5s/10s/30s, and the `-service run` verb the SCM launches. `status`
  uses a read-only SCM handle so it works **unelevated**; the mutating verbs need
  administrator rights and say so. A service has no desktop, so service mode is always
  headless and keeps its data in `%ProgramData%\PrintBridge`. `setup.iss` now registers
  the service, requires admin, and uninstalls it, replacing the HKCU autostart entry.
  **Remaining:** an explicit single-instance lock file (today the SCM prevents duplicate
  services and a second console run exits on the TCP bind), and `-Silent` switches for
  unattended installer runs.
- **Config file — ✅ implemented** (`agent/pkg/config`): `config.json` with
  server / security / queue / updater / relay sections, created with defaults, tolerant
  of a UTF-8 BOM, **recovered** rather than fatal when unparseable
  (`config.corrupt.<timestamp>.json`), clamped with logged notes, written atomically,
  and layered under the flags so a file setting wins unless the operator typed the flag.
- **Auto-update is opt-in — ✅ implemented** (`-auto-update` / `updater.auto_update`).
- **Port fallback** + `active_port.txt` + service identity in `/health` (neither the
  SDK nor the installer should ever assume a port).
- **Docs suite — ✅ implemented.** `docs/README.md` (index with an audience table and the
  three things that cause most support calls), `docs/install.md` (paths, service verbs,
  five-step verification, first printer setup, network printers, upgrade/repair, uninstall,
  handover checklist), `docs/daily-use.md` (dashboard, printer states, roles, queue
  operations, drawer, config changes, logs, weekly check), `docs/troubleshooting.md`
  (symptom-first, ending in what to capture when escalating), `docs/web-integration.md`
  (SDK and raw protocol, error codes, idempotency, origin allowlist, LAN access), and
  `docs/handover.md` (done, deliberately deferred with reasons, known debt, next phases).
  `agent/scripts/deep_qa.ps1` + `deep_qa.mjs` run the whole documented flow against a
  throwaway agent through the built SDK and report per-check pass/fail; they are safe on a
  live machine because they never touch a real printer unless `-Printer` is given.
  While writing the logs section this also surfaced and fixed a real gap: a Windows service
  had no readable log at all, so service mode now writes
  `%ProgramData%\PrintBridge\printbridge.log` (rotated once at 5 MB).

## 4. P3 — Relay as the durable path

**Done.** Targeted the Cloudflare 2026-07-09 change first: `new_sqlite_classes` in
`relay/wrangler.toml` (new KV-backed Durable Object namespaces can no longer be created,
and the Free plan only ever supported SQLite-backed DOs). Then hibernatable WebSockets
(`state.acceptWebSocket`), per-agent secret verification on `/ws/agent/:id`, an API key on
`/v1/agents/:id/print`, job acknowledgements back to the backend, wrangler v4 +
`tsconfig.json` + `@cloudflare/workers-types`, and a `wrangler deploy --dry-run` check.

What the increment actually changed:

- **Auth on both sides, failing closed.** The Worker authenticates *before* forwarding to a
  Durable Object. Agents present `Authorization: Bearer <secret>` and the relay compares
  the SHA-256 of it against the hash in an `AGENT_SECRETS` KV namespace (constant-time
  compare, unknown agent id answering exactly like a wrong secret). Backends present
  `X-API-Key`. Missing configuration produces `503` with the reason, never an open relay.
- **Hibernatable sockets.** The old code held the agent's `WebSocket` in a field and used
  `server.accept()`, which pins the object in memory and drops the connection when it is
  evicted. It now uses `acceptWebSocket` plus `webSocketMessage`, so an idle till costs
  almost nothing.
- **Per-job storage and acknowledgements.** Jobs are keyed (`job:<id>`) instead of living in
  one array in a single `pending_queue` value, are written *before* being sent, and are only
  considered done once the agent acknowledges. Two acks: `accepted` (with the agent's local
  job id, so re-sends stop) and `completed` (success or the printer's error). Delivery is
  at-least-once by design, documented as such.
- **New endpoint** `GET /v1/agents/:id/jobs/:jobId`, and `GET /status` now reports
  `agent_version`, `agent_connected_at`, per-status counts and the 20 most recent results.
- **Agent side**: acks both stages, `hello` with the version, 30 s pings with a 90 s read
  deadline, capped exponential reconnect backoff (3 s → 60 s), one serialised writer, and a
  refusal that logs the relay's own explanation instead of gorilla's bare "bad handshake".
- **Storage bounds**: 24 h TTL and a 500-job cap per agent, pruned on write.
- **Tests**: `relay/src/lib.test.ts` (12 assertions, `node --test` with Node's built-in type
  stripping — no framework, no new dependency) and two Go tests covering the ack lifecycle
  and the refusal path. Verified: `npx tsc --noEmit` clean, 12/12 relay tests, 14/14 Go
  packages, and `wrangler deploy --dry-run` reporting both bindings.

Writing the tests caught a real bug: the base64 validator stripped *all* whitespace, so
`"has spaces"` was accepted as base64. It now tolerates only line breaks.

Still open (see `handover.md`): per-tenant keys, job scoping per backend, rate limiting.

---

## 5. Run & verify

```powershell
# Agent (headless) — ws://localhost:9567, dashboard http://localhost:9567/dashboard
cd agent; go run . -headless
# Agent with tray
cd agent; go run .
# Agent for a LAN/HTTPS-hosted POS origin, with TLS for wss://
cd agent; go run . -allow-origin https://pos.example.com -tls-cert cert.pem -tls-key key.pem

# Hardware smoke test (bypasses the OS print dialog)
cd agent; go run ./cmd/printtest -list
cd agent; go run ./cmd/printtest -printer "<name>" -escpos

# Tests
cd agent; go build ./...; go vet ./...; go test ./...
cd agent; go test -bench=. -benchmem ./pkg/queue

# SDK (build the UMD bundle the example page loads)
cd sdk; npm ci; npx tsc --noEmit; npm run build; npm pack --dry-run

# Relay
cd relay; npm install; npm run check          # tsc --noEmit + node --test
cd relay; npm run deploy:dry-run              # bundles and prints the bindings
cd relay; npx wrangler dev                    # local worker with a local DO
```

Browser-side checks (run on the POS origin, not from `file://`):

```js
// 1. Is the agent reachable at all?
await fetch('http://localhost:9567/dashboard').then(r => r.status); // 200
// 2. Will Chrome ask for local-network permission?
await navigator.permissions.query({ name: 'local-network-access' });
// 3. Pairing must NOT hand you the code:
//    ws.send({version:1,id:'1',type:'request_pairing',payload:{app_name:'x'}})
//    -> {"code_requested":true,...}  and never a "code" field
```

## 6. Dependencies & CI

| Package | In repo | Latest (2026-09-30) | Note |
|---|---|---|---|
| `fyne.io/systray` | v1.11.0 | v1.12.2 | requires cgo — see the build-tag note below |
| `modernc.org/sqlite` | v1.55.0 | v1.60.1 | keeps Windows builds cgo-free |
| `go.bug.st/serial` | v1.6.4 | v1.8.0 | |
| `gorilla/websocket` | v1.5.3 | v1.5.3 | current, but effectively frozen since 2024; consider `github.com/coder/websocket` |
| `wrangler` | ^3.100.0 | 4.144.0 | Node >= 22, new config schema |
| `typescript` | ^5.8.0 | 7.0.2 | major — upgrade separately and re-check tsup |
| `actions/setup-go` | v5 | v7 | read the version from `agent/go.mod` instead of hardcoding |
| `go` directive | `1.25.0` | — | pin a matching `toolchain` directive |

CI problems to fix (all verified):

- The workflow triggers on `main`/`develop`, but the default branch is `master`, so
  it never runs on pushes.
- `go-version: '1.22'` against `agent/go.mod`'s `go 1.25.0` silently pulls a
  different toolchain — use `go-version-file: agent/go.mod`.
- The release job cross-compiles `darwin-*` and `linux-arm64` from `ubuntu-latest`,
  but `fyne.io/systray` requires cgo and `CGO_ENABLED` defaults to 0 when
  cross-compiling. Put the tray behind a `tray` build tag (headless stays pure Go)
  and build darwin on a macOS runner.
- `docs/release-notes.md` and `docs/release-manifest.json` do not exist although the
  release job references them; `updater.DefaultManifestURL` points at
  `releases.printbridge.dev`, a domain this project does not control.

## 7. Licensing & attribution

XP Thermal Service is MIT-licensed. Behaviour, APIs and operational knowledge are
not copyrightable and everything here is an independent Go/TypeScript
implementation — but if any text, comment or constant is adapted from it, add a
`NOTICE` crediting XP Thermal Service (MIT). The same rule already applies to
QZ Tray (`PrintBridge_Agent_Instructions.md` → "Notes on Naming & IP").

