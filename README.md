# PrintBridge

A lightweight, high-reliability local print & device bridge for web applications and desktop clients — a modern alternative to QZ Tray.

## Key Features

- **Local Print Agent**: Native Go background service bypassing OS print dialogs for raw print streams (ESC/POS, ZPL, EPL).
- **Persistent Job Queue**: SQLite-backed transactional queue with exponential backoff retry — zero job loss on hardware failures or agent restarts.
- **WebSocket Bridge**: Versioned JSON WebSocket API (`ws://localhost:9567`) for browser integrations.
- **Developer-Friendly Security**: 6-digit pairing code + HMAC-signed token handshake — no complex certificate authority setup.
- **Hardened Transport**: origin allowlist, per-IP rate limiting, token origin binding, and audit trail of every rejected request.
- **Device Support**: Raw printing, cash drawer kick, serial scale read, and USB/HID scanner integration.
- **TypeScript Web SDK**: Promise-based API (`printbridge.js`) with auto-reconnect and event listeners (ESM + UMD).
- **Cloud Relay (Optional)**: Cloudflare Worker + Durable Objects tier for remote print dispatch, with per-agent secrets, hibernatable sockets, queued jobs and two-stage acknowledgements (accepted, then printed). See [docs/relay.md](docs/relay.md).
- **Auto-Update**: Periodic checks against a release manifest with SHA256 verification and rollback support.
- **Enterprise Security**: TLS/WSS support, IP-based rate limiting, and SQLite audit trail logging.
- **System Tray App**: Cross-platform tray with status indicators, dashboard launcher, and auto-start toggle.

## Monorepo Structure

```
print-bridge/
├── agent/       # Go local agent (binary, packages)
├── sdk/         # TypeScript Web SDK (npm package)
├── relay/       # Cloudflare Worker + Durable Objects
├── examples/    # Runnable integration example page (checkout-print.html)
├── docs/        # Architecture & integration docs
└── .github/     # CI/CD workflows
```

## Quick Start

### Go Agent

```bash
cd agent
go run . -headless          # console, config created next to the database
go run .                    # with the system tray icon
```

Flags:

| Flag | Default | Description |
|------|---------|-------------|
| `-port` | `9567` | WebSocket & Dashboard port |
| `-db` | `%ProgramData%\PrintBridge\printbridge.db` (service) or the per-user config dir | SQLite database path |
| `-config` | `config.json` next to the database | Configuration file path |
| `-service` | `""` | Windows service control: `install`, `uninstall`, `start`, `stop`, `status`, `repair` |
| `-headless` | `false` | Run without system tray |
| `-auto-update` | `false` | Download and apply agent updates automatically |
| `-tls-cert` | `""` | TLS certificate path (enables WSS) |
| `-tls-key` | `""` | TLS private key path |
| `-allow-origin` | loopback only | Extra browser origin allowed to connect (repeatable, e.g. `-allow-origin https://pos.example.com`) |
| `-trust-proxy` | `false` | Trust `X-Forwarded-For` for client IP attribution (only behind a local reverse proxy) |
| `-relay-url` | `""` | Cloud Relay WebSocket URL |
| `-agent-id` | `""` | Registered agent ID for relay |
| `-api-key` | `""` | Per-agent secret used to authenticate the relay connection (only its SHA-256 is stored, in the relay's `AGENT_SECRETS` KV namespace) |

## Configuration file

Settings live in `config.json`, which the agent creates with sensible defaults on first
run. **A flag the operator did not type never overrides the file**, so the file is a
real configuration source rather than documentation.

```json
{
  "server": { "port": 9567, "tls_cert_file": "", "tls_key_file": "" },
  "security": { "allowed_origins": ["https://pos.example.com"], "trusted_proxy": false },
  "queue": { "max_attempts": 5, "max_depth": 5000, "max_concurrent_jobs": 4 },
  "updater": { "auto_update": false, "manifest_url": "" },
  "relay": { "url": "", "agent_id": "", "api_key": "" }
}
```

- A **missing** file is created with defaults.
- A **UTF-8 BOM** (Notepad, PowerShell redirection) is tolerated.
- An **unparseable** file is copied to `config.corrupt.<timestamp>.json` and defaults
  are used: a typo in a JSON file must never stop a till printing. The original file is
  left untouched so it can be fixed and restored.
- Out-of-range values are clamped, and each clamp is logged.
- Writes are atomic (temp file plus rename), so a crash mid-write cannot leave a
  half-written file behind.

## Running as a Windows service

```powershell
# Needs administrator rights to register the service (status does not).
printbridge-agent.exe -service install      # auto-start, depends on the Spooler, restarts on failure
printbridge-agent.exe -service start
printbridge-agent.exe -service status       # works unelevated
printbridge-agent.exe -service stop
printbridge-agent.exe -service uninstall    # database and config are left in place
```

- `-service install` also acts as **repair**: an existing installation is replaced and
  the configuration file is never touched.
- The service runs as `LocalSystem`, headless (a service has no desktop for a tray
  icon), with the print spooler as a dependency, and SCM recovery actions of
  restart-after-5s/10s/30s so a 3am crash heals by itself.
- Service data lives in `%ProgramData%\PrintBridge` (`printbridge.db`, `config.json`).
  Console mode keeps using the per-user config directory.
- The installer (`agent/installer/windows/setup.iss`) registers the service and grants
  users modify rights on that directory.


### TypeScript SDK

```bash
cd sdk
npm install
npm run build
```

## WebSocket Protocol

All messages follow `{"version":1,"id":"...","type":"...","payload":{...}}`.

Every message type except `ping`, `request_pairing`, `confirm_pairing` and
`authenticate` requires a paired token. The token can be sent once with
`authenticate` (recommended) or inline in the `payload.token` field of the
request itself. Unauthenticated requests are answered with
`{"success":false,"error":"pairing_required"}` and written to the audit log.

| Type | Auth required | Payload | Response |
|------|---------------|---------|----------|
| `ping` | no | `{}` | `{"status":"pong"}` |
| `request_pairing` | no | `{"app_name"}` | `{"code_requested":true,"expires_at":"..."}` |
| `confirm_pairing` | no | `{"code"}` | `{"token":"pb_tok_..."}` |
| `authenticate` | no | `{"token"}` | `{"authenticated":true}` |
| `list_printers` | yes | `{}` | `{"printers":[...]}` |
| `print` | yes | `{"printer"` or `"role"`, `"data"` (base64)`, "job_name", "priority"?, "idempotency_key"?}` | `{"job_id","status","created","duplicate"}` |
| `queue_status` | yes | `{}` | `{"paused":false,"counts":{...}}` |
| `pause_queue` | yes | `{}` | `{"paused":true}` |
| `resume_queue` | yes | `{}` | `{"paused":false}` |
| `retry_job` | yes | `{"job_id"}` | `{"job_id","status":"queued"}` |
| `clear_failed_jobs` | yes | `{}` | `{"removed":N}` |
| `job_status` | yes | `{"job_id"}` | full job object |
| `get_printer_health` | yes | `{}` | `{"printers":[...]}` |
| `open_drawer` | yes | `{"printer"` or `"role"`, `"pin"` (2\|5)`, "brand"}` | `{"status":"drawer_kicked"}` |
| `list_printer_roles` | yes | `{}` | `{"roles":[...]}` |
| `assign_printer_role` | yes | `{"role","printer","label"?,"capabilities"?}` | the stored assignment |
| `remove_printer_role` | yes | `{"role"}` | `{"role":"kitchen"}` |
| `test_print` | yes | `{"printer"` or `"role"`, `"slip"?:receipt\|kitchen\|text}` | `{"job_id","printer","slip","bytes"}` |
| `diagnose_printer` | yes | `{"printer"` or `"role"}` | `{"printer","installed","checks":[...]}` |
| `list_serial_ports` | yes | `{}` | `{"ports":[...]}` |
| `diagnose_printers` | yes | `{}` | `{"summary":"...","resolutions":[...]}` |
| `list_hid_devices` | yes | `{}` | `{"devices":[...]}` |
| `check_update` | yes | `{}` | `{"available":bool,"available_version":"..."}` |
| `apply_update` | yes | `{}` | `{"message":"..."}` |

Push events (`job_status_changed`, `printer_status_changed`) are only sent to
clients that have already authenticated.

### Pairing is out-of-band by design

The 6-digit code is **never returned over the WebSocket**. It is displayed in the
agent tray icon and on the dashboard (`http://localhost:9567/dashboard`), and the
human types it into the web app. Returning it over the socket would let any page
pair itself with no user involvement.

### Browser access: origin allowlist and Local Network Access
- **Origin allowlist.** Loopback origins (`http://localhost:<any port>`,
  `http://127.0.0.1:<any port>`, and the `https://`/`[::1]` equivalents) are
  trusted by default. Any other origin must be added with `-allow-origin`, which
  also accepts host patterns such as `http://pos.local:*`. Connections from a
  disallowed origin are refused with HTTP 403 before the WebSocket upgrade.
- **Chrome Local Network Access.** Chrome 142+ gates fetch/XHR to localhost behind
  a permission prompt, and Chrome 147+ extends that same permission to
  **WebSockets**. A POS UI served from a public HTTPS origin therefore asks the
  user to grant local network access before the first connection. Pages served
  from `http://localhost`, `127.0.0.1`, or a private IP talking to the same
  address space are exempt, and Firefox/Safari do not enforce it yet. Managed
  fleets can pre-grant the `LocalNetworkAccessAllowedForUrls` policy for the POS
  origin. See the SDK guide for detection helpers (`permissions.localNetwork()`,
  `diagnose()`).
- **HTTPS pages must use `wss://`.** Start the agent with `-tls-cert`/`-tls-key`;
  the SDK's `secure` option defaults to `true` when the page is HTTPS.

## Health endpoint

`GET /health` returns the agent's own verdict. It is deliberately **not** a liveness
check: an agent that is running but cannot print anything is `degraded`, not
`healthy`.

```json
{
  "service": "printbridge-agent",
  "version": "0.1.0",
  "status": "degraded",
  "reasons": [
    "printer \"USB Receipt\" is unknown: Status unverified (spooler reports offline) (spooler flag not corroborated - a successful print will clear this)"
  ],
  "port": 9567,
  "uptime_seconds": 412,
  "snapshot_age_ms": 1200,
  "printers": ["..."],
  "queue": { "queued": 0, "sending": 0, "success": 12, "failed": 1, "retrying": 0, "total": 13 }
}
```

- `healthy` has a usable printer, `degraded` is running but cannot currently
  complete work, `unhealthy` cannot accept work at all (answered as HTTP 503).
- `service` identifies the real agent, so a client probing a port range can tell it
  apart from whatever else is listening.
- `reasons` is written to be read by a support engineer — it is the answer, not a hint.
- `/health` needs no pairing (installer checklists and monitoring probes depend on
  it) but is **not** CORS-enabled and rejects foreign `Host` headers, so a browser
  page cannot read it cross-origin.

```bash
curl -s http://localhost:9567/health
```

## Printer status semantics

PrintBridge never trusts a single spooler signal. On Windows the `WorkOffline`
attribute is routinely stale on USB thermal printers — a device that prints Windows
test pages perfectly still reports `WorkOffline: True` — so an offline verdict
requires corroboration.

| Evidence | Verdict |
|---|---|
| Hardware fault: jam, paper out, no toner, door open, user intervention, printer error | `offline` |
| A raw print completed inside the 2-minute trust window | `online` — overrides the offline flags |
| Queue paused, or the printer's port does not exist | `offline` |
| Offline flags only, with no corroboration | `unknown` — reported with an explanation, never as a broken printer |
| Nothing recognised (status code 1 "Other" or 2 "Unknown" on cheap units) | `online` ("Ready") |

Printer enumeration is also shared: one cached snapshot (~4s TTL, single-flight)
serves the health endpoint, telemetry, the dashboard and the `list_printers` RPC, so
a health sweep across N printers costs one OS call rather than one per consumer.

## Following the cable

Moving a USB printer to a different socket mints a brand new Windows port
(`USB003` → `USB011`) while the print queue stays pinned to the old one, which
silently breaks printing. PrintBridge records where each device-backed queue lives
and reports when it has moved:

- At startup the agent records every queue's port together with the stable device
  identity (for example `VID_04B8&PID_0E15&MI_00`) read from
  `HKLM\SYSTEM\CurrentControlSet\Enum\<bus>\<hardware id>\<instance>\Device Parameters\PortName`.
- `diagnose_printers` returns one resolution per queue: `none` (port present),
  `rebind` (the device reappeared on exactly one other port), `ambiguous` (several
  ports could host it — the agent refuses to guess), or `missing`.
- A port already owned by another configured queue is never taken over, so a kitchen
  ticket cannot be hijacked by the receipt printer.
- Network and virtual ports (`IP_…`, `WSD-…`, `PORTPROMPT:`) are not tracked because
  they do not follow movable hardware.

```json
{
  "summary": "Kitchen: device VID_04B8&PID_0E15 moved from USB003 to USB011",
  "resolutions": [
    { "printer_name": "Kitchen", "action": "rebind", "current_port": "USB003",
      "new_port": "USB011", "reason": "device VID_04B8&PID_0E15 moved from USB003 to USB011" }
  ]
}
```

The agent currently **detects and reports** port migration; it does not yet rewrite
the spooler's port binding. That write needs a `GetPrinter`/`SetPrinter` level-2
round trip (the `Attributes`/`PortName` block is self-referential) validated against
real hardware before it is allowed anywhere near a customer's spooler.

## Network printers (TCP 9100)

Queues whose Windows spooler port is a TCP address (`IP_192.168.1.100`, `10.0.0.5:9100`,
`printer.local`) are written to **directly** over their RAW/JetDirect port instead of
going through the spooler. That removes a class of spooler-only failure modes — stale
offline flags, paused queues, driver rendering — and needs no printer driver at all.

- Detection is automatic from the port name; there is nothing to configure.
- Health uses a real TCP probe: a printer that answers is reported `online` even when
  the spooler claims otherwise, and one that refuses is `offline` ("Not reachable").
- If no connection can be established at all, the job falls back to the spooler queue.
  After a **partial** write it deliberately does *not* fall back, because printing the
  job again would duplicate a receipt.
- `list_printers` and `get_printer_health` report `type: "network"` with
  `network_address: "host:port"`.
- Device (`USB001`), virtual (`PORTPROMPT:`, `SHRFAX:`) and unrecognised (`WSD-…`,
  `URL_…`) ports keep the spooler path, so nothing changes for existing installs.

## The job queue

Every accepted job is written to SQLite before it is dispatched, so an agent restart
never loses one:

- **Scheduled retries, not sleeps.** A failed attempt records `next_attempt_at` with
  exponential backoff (1s, 2s, 4s, 8s, then capped at 10s). The worker never sleeps
  on a single job, so one struggling printer cannot delay another station.
- **Parallel per printer.** Printers are served in parallel (4 by default) while each
  individual printer runs one job at a time, which keeps per-station order. A printer
  whose socket hangs no longer stalls the tills.
- **Priority.** `priority` orders the queue (higher first, then oldest first), so an
  order ticket can overtake a queued report.
- **Idempotency.** Supply `idempotency_key` and a retried POS request returns the
  original job (`"duplicate": true`) instead of printing a second receipt.
- **Depth limit.** The queue refuses new work past `maxDepth` (5000 by default) with
  `print queue is full` rather than growing without bound while no printer works.
- **Dead-letter + retry.** A job that exhausts its attempts lands in the dead-letter
  state (`failed`), which `/health` reports and counts against the agent's verdict.
  `retry_job` puts it back with a clean attempt count; `clear_failed_jobs` discards it.
- **Pause / resume.** `pause_queue` stops dispatch without dropping work, which is
  what you want while swapping hardware.
- **Crash recovery.** Jobs left mid-flight are requeued on startup with their retry
  schedule cleared.

```js
await pb.print('Kitchen Printer', escpos, { jobName: 'Order #1234', idempotencyKey: 'order-1234' });
const { counts, paused } = await pb.queue.status();
```


Printer ids are **stations, not devices**. `receipt` and `kitchen` are what a POS
addresses; which physical printer fills the role is configuration.

- `assign_printer_role` points a role at a printer, optionally with a capability
  profile (`max_width: 32` for 58mm paper, plus cut/bold/cash-drawer support).
- `print`, `open_drawer`, `test_print` and `diagnose_printer` all accept either
  `printer` or `role`.
- `list_printer_roles` reports each role with its live resolution, so a dashboard can
  show "kitchen → not installed" before a shift starts.
- Replacing a failed printer is a **role reassignment** — the POS needs no change.
- `test_print` sends a built-in ESC/POS slip (receipt, kitchen or plain text) built
  from the role's capability profile. The agent never emits a cut or bold sequence a
  printer does not claim to support.

```js
await pb.roles.assign('kitchen', 'XP-80C @ Kitchen', { label: 'Kitchen', capabilities: { max_width: 32 } });
await pb.printToRole('kitchen', escposBytes, { jobName: 'Order #1234' });

const [{ role, resolved, status }] = await pb.roles.list();
const diagnosis = await pb.diagnosePrinter({ role: 'kitchen' });
console.log(diagnosis.checks.join('\n'));
```

Job names are recorded as the *resolved* device at enqueue time, so a reassignment
affects subsequent jobs rather than ones already queued.

## Documentation

| Document | Read it when |
|---|---|
| [docs/README.md](docs/README.md) | You want the index and the three things that cause most support calls |
| [docs/install.md](docs/install.md) | Deploying to a till, verifying an install, upgrading, handing over |
| [docs/daily-use.md](docs/daily-use.md) | Running the dashboard, assigning stations, routine operation |
| [docs/troubleshooting.md](docs/troubleshooting.md) | Something is broken — symptom-first |
| [docs/web-integration.md](docs/web-integration.md) | Connecting your own web app |
| [docs/integration-walkthrough.md](docs/integration-walkthrough.md) | How it works, and the step-by-step integration for a specific site |
| [examples/checkout-print.html](examples/checkout-print.html) | A runnable checkout page (open it on a till) |
| [docs/architecture.md](docs/architecture.md) | Understanding the queue, transports, health and security model |
| [docs/sdk_guide.md](docs/sdk_guide.md) | The SDK API reference |
| [docs/relay.md](docs/relay.md) | Deploying the cloud relay and printing to a till you cannot reach |
| [docs/handover.md](docs/handover.md) | Picking up development |
| [docs/implementation/update-plan.md](docs/implementation/update-plan.md) | The prioritised work plan |

```powershell
# End-to-end smoke test: starts a throwaway agent and checks the whole protocol
powershell -ExecutionPolicy Bypass -File agent/scripts/deep_qa.ps1
```

## License

SDK and public tools are licensed under the [MIT License](LICENSE).
