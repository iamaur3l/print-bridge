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

1. **Request**: Web app calls `request_pairing` → agent generates secure 6-digit code (valid 5 min).
2. **Display**: Code shown in the tray UI / dashboard (out-of-band human channel).
3. **Confirm**: User enters code into web app → `confirm_pairing` → agent issues HMAC-signed token.
4. **Authenticate**: Subsequent connections present the token in `authenticate` message.
5. **Revoke**: Tokens can be revoked from the dashboard, immediately invalidating future connections.
6. **Rate limit**: Max 5 code attempts per session; 10 pairing requests per minute per IP.

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
| is_online | INTEGER | 1 if reachable |
| status_description | TEXT | Driver status string |
| last_seen_at | DATETIME | Last poll timestamp |

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

The relay is a Cloudflare Worker fronting Durable Objects. Each agent gets a dedicated Durable Object that holds a persistent WebSocket connection from the agent and provides a REST API for backend services:

- `POST /v1/agents/:agentId/print` — enqueue a job on the remote agent
- `GET /v1/agents/:agentId/status` — check agent online/offline state

The relay is optional and opt-in. When enabled, the agent maintains an outbound WebSocket to the relay, bypassing NAT/firewall issues.

## Performance Design

- **SQLite WAL mode**: Write-Ahead Logging for concurrent read/write throughput.
- **sync.Pool buffer reuse**: Zero-alloc byte buffers for high-frequency printing.
- **Rate limiting**: Per-IP request limiting (60 req/min general, 10 req/min for pairing).
- **Graceful shutdown**: In-flight jobs complete or persist before exit.
