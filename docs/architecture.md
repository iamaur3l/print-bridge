# PrintBridge Architecture

PrintBridge is a standalone device bridge connecting web applications to physical POS hardware (thermal printers, cash drawers, scales, barcode scanners) via a local Go agent with an optional cloud relay tier.

## Architecture Diagram

```
                        +-------------------------+
                        |   Web Application UI    |
                        +------------+------------+
                                     | WebSocket (ws://localhost:9567)
                                     v
+------------------------------------+------------------------------------+
|                           LOCAL AGENT (Go)                             |
|                                                                        |
|  +-------------------+    +-------------------+    +----------------+  |
|  | WebSocket Server  |    |  Pairing / Auth   |    | Tray App / UI  |  |
|  | (gorilla/websocket)|    |  (HMAC + tokens)  |    | (fyne.io/systray)| |
|  +--------+----------+    +---------+---------+    +-------+--------+  |
|           |                        |                        |         |
|           +----------+-------------+------------------------+         |
|                      |                                                  |
|                      v                                                  |
|           +---------------------------+                                 |
|           |   SQLite Job Queue        |  (modernc.org/sqlite, WAL mode) |
|           |   - Persistent queue      |                                 |
|           |   - Exponential backoff   |                                 |
|           |   - Audit trail logging   |                                 |
|           +-------------+-------------+                                 |
|                         |                                               |
|                         v                                               |
|           +---------------------------+                                 |
|           |   Device I/O Layer        |                                 |
|           |   (printer, drawer,       |                                 |
|           |    serial, HID)           |                                 |
|           +-------------+-------------+                                 |
+-------------------------+-----------------------------------------------+
                          |
          +---------------+---------------+
          |               |               |
          v               v               v
    [WinSpool/CUPS]   [Serial Port]   [USB HID]
    Printers          Scales          Scanners/Drawers


                        +---------------------------+
                        |   CLOUD RELAY (Optional)  |
                        |   Cloudflare Worker + DO  |
                        +------------+--------------+
                                     | WSS (outbound from agent)
                                     v
                        +---------------------------+
                        |   Backend Services        |
                        |   POST /v1/agents/:id/print|
                        +---------------------------+
```

## WebSocket RPC Protocol

All messages use a versioned JSON envelope:

```json
{"version": 1, "id": "req_001", "type": "print", "payload": {...}}
```

Messages are request-response with correlating `id` fields. Push events (`job_status_changed`, `printer_status_changed`) are sent without an `id`. The protocol is documented in the SDK guide and README.

## Pairing Code Security Model

1. **Request**: Web app calls `request_pairing` → agent generates a secure 6-digit code (valid 5 min).
2. **Display**: the code is shown in the tray UI and on the local dashboard. It is **never returned over the WebSocket** to the requesting client — that is what makes the confirmation step meaningful.
3. **Confirm**: the human reads the code out-of-band and types it into the web app → `confirm_pairing` → agent issues an HMAC-signed token.
4. **Authenticate**: subsequent connections present the token via `authenticate` (or inline as `payload.token`), and every message type other than `ping`/pairing/`authenticate` is rejected until they do.
5. **Origin binding**: a token is only valid for the origin it was issued to, so a token leaked to (or stolen by) another web application cannot be replayed.
6. **Revoke**: tokens can be revoked from the dashboard, immediately invalidating future connections.
7. **Origin allowlist**: only loopback origins are trusted by default; other origins must be added with `-allow-origin`, and refused connections get HTTP 403 before the upgrade.
8. **Rate limit**: max 5 code attempts per session; 10 pairing requests per minute per origin; 60 connections per minute per IP.
9. **Audit**: every rejected request, token rejection (including cross-origin replay), pairing, draw-kick and print job is written to `audit_logs` with the client IP.

## SQLite Schema

### `jobs`
| Column | Type | Description |
|--------|------|-------------|
| id | TEXT PK | UUID v4 |
| printer_name | TEXT | Target printer |
| job_name | TEXT | Human-readable name |
| payload | BLOB | Raw print bytes |
| status | TEXT | queued/sending/success/failed/retrying |
| created_at | DATETIME | Job creation time |
| last_attempt_at | DATETIME | Last delivery attempt |
| attempts | INTEGER | Retry count |
| max_attempts | INTEGER | Max retries (default 5) |
| error_message | TEXT | Last error detail |
| priority | INTEGER | Higher runs sooner (default 0) |
| idempotency_key | TEXT | Unique when set; deduplicates retried requests |
| next_attempt_at | DATETIME | When a retrying job becomes eligible again |

### `paired_apps`
| Column | Type | Description |
|--------|------|-------------|
| id | TEXT PK | UUID v4 |
| app_name | TEXT | Human-readable app name |
| origin | TEXT | Web origin URL |
| token | TEXT | HMAC-signed auth token |
| created_at | DATETIME | Pairing time |
| last_used_at | DATETIME | Last token validation |

### `printer_telemetry`
| Column | Type | Description |
|--------|------|-------------|
| printer_name | TEXT PK | Printer identifier |
| driver_name | TEXT | OS driver name |
| port_name | TEXT | Connection port |
| is_online | INTEGER | 1 if usable |
| state | TEXT | Corroborated verdict: online / offline / unknown |
| status_description | TEXT | Driver status string |
| status_detail | TEXT | Why that verdict was reached |
| last_seen_at | DATETIME | Last poll timestamp |

## Health & printer status

`GET /health` reports the agent's own verdict (`healthy`, `degraded`, `unhealthy`)
plus plain-language `reasons`, the queue counters, and the printer list. It is not a
liveness check: an agent that is running but has no usable printer is `degraded`, and
an agent that cannot accept work at all answers HTTP 503.

Printer verdicts are corroborated rather than read straight off the spooler:

- A hardware fault (jam, paper out, no toner, door open, user intervention, error) is
  `offline`. `WritePrinter` succeeds into the queue even when the device cannot print,
  so a fault outranks a recent write.
- A raw print that completed inside the 2-minute trust window means `online` and
  overrides the spooler's offline flags.
- The `WorkOffline` attribute and `PRINTER_STATUS_OFFLINE` bit on their own are not
  enough — on USB thermal printers they are frequently stale. That becomes `unknown`
  with an explanation instead of a false `offline`.
- Unrecognised status codes count as ready, never as errors.

Enumeration is shared through one cached snapshot (~4s TTL, single-flight) so a
health sweep across N printers costs one OS call, not one per consumer.

## Port bindings ("following the cable")

Windows mints a new port for every physical USB socket, while the print queue stays
pinned to whichever port existed at install time. The agent records each
device-backed queue's port plus its stable device identity
(`Enum\<bus>\<hardware id>\<instance>\Device Parameters\PortName`) in the
`printer_bindings` table, and `PlanPortBindings` decides what to do when a port
disappears:

| Situation | Action |
|---|---|
| Configured port is present | `none` |
| Device reappeared on exactly one other port | `rebind` (proposed, not applied) |
| Several ports could host the device (identical units on one hub) | `ambiguous` — refuse to guess |
| Candidate port already owned by another queue | `missing` — never taken over |
| Device gone, or no identity ever recorded | `missing` |

The same rules are exposed over the WebSocket as `diagnose_printers` (auth
required) and logged at startup. Applying a `rebind` to the spooler is deliberately
not implemented yet.

## Transports

`PrintRaw` has two backends, chosen per queue by `Router`:

| Queue port looks like | Transport | Why |
|---|---|---|
| `IP_192.168.1.100`, `10.0.0.5:9100`, `printer.local` | Direct TCP RAW (`SendRawToNetwork`) | No spooler, no driver, and the printer can be probed for real reachability |
| `USB001`, `LPT1`, `DOT4_001`, `PORTPROMPT:`, `SHRFAX:`, `WSD-…`, `URL_…`, anything unrecognised | OS spooler (`winspool` / CUPS `lpr`) | The historical path; unchanged |

A network job falls back to the spooler only when the connection was never
established (`PartialWriteError` with zero bytes written); after a partial write the
error is returned so the queue retries rather than printing the receipt twice. The
same write records ground truth for the health engine.

## Configuration and process model

Settings come from `config.json`, layered with flags: a flag only overrides the file
when it was actually typed (`flag.Visit`), so the file is the configuration and the
flags are overrides. The file is recovered rather than fatal when unparseable.

The agent has two process models:

| Mode | Started by | Data directory | UI |
|---|---|---|---|
| Console | a person, `systemd`, `launchd` | per-user config dir | tray icon unless `-headless` |
| Windows service | the Service Control Manager (`-service run`) | `%ProgramData%\PrintBridge` | none: session 0 has no desktop |

The service declares the print spooler as a dependency, registers recovery actions
(restart after 5s/10s/30s), and reports `StopPending`/`Stopped` around a graceful
drain so the queue is not abandoned mid-job. `-service status` uses a read-only SCM
handle so it works without administrator rights.

## The queue

`jobs` is the single source of truth: a job is written before it is dispatched and its
state transitions are recorded as they happen (`queued` → `sending` → `success`, or
`retrying` with a `next_attempt_at`, or `failed` as the dead-letter state).

| Behaviour | How |
|---|---|
| Ordering | `ORDER BY priority DESC, created_at ASC` |
| Retry policy | Exponential backoff via `BackoffFor` (1s→10s cap) written to `next_attempt_at`; the worker loop only ticks, it never sleeps on a job |
| Parallelism | One job per printer at a time (so per-station order holds) with up to `maxConcurrent` printers in flight |
| Deduplication | `idempotency_key` with a partial unique index; a repeat returns the original job |
| Backpressure | `maxDepth` unfinished jobs, then `ErrQueueFull` |
| Dead letter | `failed` after `maxAttempts`, reported by `/health`, requeued by `retry_job` |
| Stop the line | `pause_queue` blocks dispatch without dropping work |
| Crash recovery | `ResetInflightJobs` requeues `sending` rows and clears their retry schedule |

## Printer roles

A job can address a **station** instead of a device. `kitchen` and `receipt` are
roles stored in `printer_roles`; the server resolves a role to a real queue name at
dispatch time, so a failed printer is replaced by reassigning the role and the POS
never changes. Capability profiles travel with the role and drive the built-in test
slips, which is why `test_print` cannot emit a cut command to a printer that does not
support one.

`diagnose_printer` composes everything the agent knows about a queue — transport,
port binding, roles, spooler verdict, last successful write — into a `checks` list
written to be read by whoever is standing at the till.

### `printer_bindings`
| Column | Type | Description |
|--------|------|-------------|
| printer_name | TEXT PK | Print queue name |
| port_name | TEXT | Last known good spooler port (e.g. `USB003`) |
| device_id | TEXT | Stable device identity observed while that port was present |
| updated_at | DATETIME | Last observation |

### `printer_roles`
| Column | Type | Description |
|--------|------|-------------|
| role | TEXT PK | Station name, normalised to lower case (`kitchen`) |
| printer_name | TEXT | The print queue that currently serves the role |
| label | TEXT | Human label for the dashboard |
| capabilities | TEXT | JSON capability profile (width, cut, bold, cash drawer…) |
| created_at | DATETIME | First assignment |
| updated_at | DATETIME | Last reassignment |

### `audit_logs`
| Column | Type | Description |
|--------|------|-------------|
| id | INTEGER PK | Auto-increment |
| event_type | TEXT | Event category |
| origin | TEXT | Request origin |
| app_name | TEXT | Paired app name |
| details | TEXT | Event details |
| ip_address | TEXT | Client IP |
| created_at | DATETIME | Event time |

## Cloud Relay Architecture

The relay is a Cloudflare Worker fronting Durable Objects. The Worker is the authenticated
front door; there is one Durable Object per agent. See [relay.md](relay.md) for the
deployment guide and the API reference.

```
backend  --(X-API-Key)-->  Worker  --(authenticated stub call)-->  DO(agentId)
                                                                    |  hibernatable socket
agent    <---------------------------(print)------------------------+
         ---------------------------(job_result)-> acked / completed
```

- `POST /v1/agents/:agentId/print` — queue a job for that agent
- `GET /v1/agents/:agentId/status` — online state, pending count, recent results
- `GET /v1/agents/:agentId/jobs/:jobId` — one job, with its local job id and error
- `GET /ws/agent/:agentId/ws` — the agent's own outbound socket

Design decisions worth knowing:

- **Authentication happens in the Worker, before the object is touched.** The DO needs no
  bindings at all, and a request that fails auth never reaches the agent's object.
- **Agent secrets are stored as SHA-256 hashes** in a KV namespace, compared in constant
  time, and an unknown agent id answers exactly like a wrong secret.
- **Fail closed.** Without the KV binding or the API key, the relay refuses everything with
  an explanation. An unconfigured relay is useless, never open.
- **Hibernatable WebSockets**, so an idle till costs almost nothing: the agent's socket
  survives the object being evicted, and `webSocketMessage` wakes it.
- **Acknowledgements drive delivery.** A job is stored before it is sent and removed only
  after the agent has acknowledged it, so a crash repeats a job instead of losing it.
  Delivery is therefore at-least-once.
- **SQLite-backed Durable Objects** (`new_sqlite_classes`): KV-backed DO namespaces can no
  longer be created, so a fresh deploy must declare the SQLite backend.
- **Bounded state**: 24 hour TTL and a 500 job cap per agent.
- **The relay is optional and opt-in.** The agent dials out only when `relay.url` is set;
  the local WebSocket path is unaffected and remains the preferred route on a LAN.


## Performance Design

- **SQLite WAL mode**: Write-Ahead Logging for concurrent read/write throughput.
- **sync.Pool buffer reuse**: Zero-alloc byte buffers for high-frequency printing.
- **Rate limiting**: Per-IP request limiting (60 req/min general, 10 req/min for pairing).
- **Graceful shutdown**: In-flight jobs complete or persist before exit.
