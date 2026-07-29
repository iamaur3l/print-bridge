# PrintBridge

A lightweight, high-reliability local print & device bridge for web applications and desktop clients — a modern alternative to QZ Tray.

## Key Features

- **Local Print Agent**: Native Go background service bypassing OS print dialogs for raw print streams (ESC/POS, ZPL, EPL).
- **Persistent Job Queue**: SQLite-backed transactional queue with exponential backoff retry — zero job loss on hardware failures or agent restarts.
- **WebSocket Bridge**: Versioned JSON WebSocket API (`ws://localhost:9567`) for browser integrations.
- **Developer-Friendly Security**: 6-digit pairing code + HMAC-signed token handshake — no complex certificate authority setup.
- **Device Support**: Raw printing, cash drawer kick, serial scale read, and USB/HID scanner integration.
- **TypeScript Web SDK**: Promise-based API (`printbridge.js`) with auto-reconnect and event listeners (ESM + UMD).
- **Cloud Relay (Optional)**: Cloudflare Worker + Durable Objects tier for remote print dispatch.
- **Auto-Update**: Periodic checks against a release manifest with SHA256 verification and rollback support.
- **Enterprise Security**: TLS/WSS support, IP-based rate limiting, and SQLite audit trail logging.
- **System Tray App**: Cross-platform tray with status indicators, dashboard launcher, and auto-start toggle.

## Monorepo Structure

```
print-bridge/
├── agent/       # Go local agent (binary, packages)
├── sdk/         # TypeScript Web SDK (npm package)
├── relay/       # Cloudflare Worker + Durable Objects
├── docs/        # Architecture & integration docs
└── .github/     # CI/CD workflows
```

## Quick Start

### Go Agent

```bash
cd agent
go run main.go
```

Flags:
| Flag | Default | Description |
|------|---------|-------------|
| `-port` | `9567` | WebSocket & Dashboard port |
| `-db` | `~/.config/PrintBridge/printbridge.db` | SQLite database path |
| `-headless` | `false` | Run without system tray |
| `-tls-cert` | `""` | TLS certificate path (enables WSS) |
| `-tls-key` | `""` | TLS private key path |
| `-relay-url` | `""` | Cloud Relay WebSocket URL |
| `-agent-id` | `""` | Registered agent ID for relay |
| `-api-key` | `""` | API key for relay auth |

### TypeScript SDK

```bash
cd sdk
npm install
npm run build
```

## WebSocket Protocol

All messages follow `{"version":1,"id":"...","type":"...","payload":{...}}`.

| Type | Payload | Response |
|------|---------|----------|
| `ping` | `{}` | `{"status":"pong"}` |
| `list_printers` | `{}` | `{"printers":[...]}` |
| `print` | `{"printer","data"(base64)}` | `{"job_id":"..."}` |
| `job_status` | `{"job_id"}` | full job object |
| `request_pairing` | `{"app_name"}` | `{"code":"123456"}` |
| `confirm_pairing` | `{"code"}` | `{"token":"pb_tok_..."}` |
| `authenticate` | `{"token"}` | `{"authenticated":true}` |
| `get_printer_health` | `{}` | `{"printers":[...]}` |
| `open_drawer` | `{"printer","pin"(2\|5)}` | `{"status":"drawer_kicked"}` |
| `list_serial_ports` | `{}` | `{"ports":[...]}` |
| `list_hid_devices` | `{}` | `{"devices":[...]}` |
| `check_update` | `{}` | `{"available":bool,"available_version":"..."}` |
| `apply_update` | `{}` | `{"message":"..."}` |

## License

SDK and public tools are licensed under the [MIT License](LICENSE).
