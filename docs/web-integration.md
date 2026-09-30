# Integrating a Web Application

Audience: the developer connecting a POS or back-office app to PrintBridge.

> **New to this?** [integration-walkthrough.md](integration-walkthrough.md) explains how the
> pieces fit together and takes you through the integration in 12 steps, with a runnable
> example page. This document is the reference for the same API.

The agent listens on `localhost` and speaks a small JSON protocol over a WebSocket. The
TypeScript SDK wraps it; this document covers both, plus the operational rules that decide
whether a till actually works.

---

## 1. Preflight: is the agent there?

```powershell
Invoke-RestMethod http://localhost:9567/health | Select-Object service, status, port
```

`service` answering `printbridge-agent` proves it is our agent and not something else on
the port. `/health` deliberately needs no pairing, so it is the right first call in a
support script.

From a **browser** `/health` is not readable cross-origin (it sets no CORS headers), so do
the equivalent check with the SDK: `pb.diagnose()` reports the URL, connection state, the
browser's local-network permission, and a plain-language hint.

## 2. Install the SDK

```bash
npm install printbridge
```

```ts
import PrintBridgeClient from 'printbridge';

const pb = new PrintBridgeClient({ host: 'localhost', port: 9567 });
```

| Option | Default | Description |
|---|---|---|
| `host` / `port` | `localhost` / `9567` | Where the agent listens |
| `autoConnect` | `true` | Connect on construction |
| `token` | `localStorage` | Persisted pairing token |
| `secure` | `true` on HTTPS pages | Use `wss://` instead of `ws://` |
| `requestTimeout` | `15000` | Reject a request that gets no answer |
| `reconnectInterval` / `maxReconnectDelay` | `3000` / `30000` | Reconnect backoff |

## 3. Pair once per origin

The pairing code appears in the agent's tray icon and on the dashboard
(`http://localhost:9567/dashboard`). It is **never returned over the socket**, so a page
cannot pair itself: a human has to read it.

```ts
const { code_requested } = await pb.pairing.request('My POS App');
const code = prompt('Enter the 6-digit code shown in the PrintBridge tray');
const { token } = await pb.pairing.confirm(code!);   // stored in localStorage
```

The token is valid **only for the origin it was issued to**. Moving the app to another
hostname means pairing again, and revoking the app in the dashboard invalidates it
immediately.

## 4. Print

```ts
// Raw bytes: you control the encoding (ESC/POS, ZPL, EPL)
const escpos = new Uint8Array([0x1B, 0x40, 0x48, 0x65, 0x6C, 0x6C, 0x6F, 0x0A, 0x1D, 0x56, 0x41, 0x00]);
await pb.print('XP-80C', escpos, { jobName: 'Order #1234' });
```

**Print to a role, not a device**, so replacing a printer never needs a code change:

```ts
await pb.printToRole('kitchen', escpos, { jobName: 'Order #1234' });
```

**Make retries safe.** A POS that retries its own request can otherwise print a second
receipt:

```ts
const { jobId, duplicate } = await pb.printToRole('receipt', escpos, {
  jobName: 'Order #1234',
  idempotencyKey: 'order-1234'   // the same key returns the original job, duplicate = true
});
```

**Urgent jobs can overtake queued ones** with `priority` (higher first, then oldest first):

```ts
await pb.printToRole('kitchen', ticket, { priority: 10 });
```

The call returns as soon as the job is **stored**, not when paper comes out. Printers are
slow and can be offline, which is the point of the queue: poll `jobs.status(jobId)` or
listen for events.

## 5. Follow the job

```ts
pb.on('job_status_changed', (job) => {
  console.log(`${job.id}: ${job.status}`);   // queued → sending → success | retrying | failed
});

pb.on('printer_status_changed', ({ printer }) => {
  console.log(`${printer.printer_name}: ${printer.state}`);   // online | offline | unknown
});

const job = await pb.jobs.status(jobId);
if (job.status === 'failed') {
  await pb.queue.retryJob(job.id);      // after fixing the cause
}
```

The same fields are on `job_status`: `attempts`, `max_attempts`, `error_message`, and
`next_attempt_at` (when a retry becomes eligible). A printer state of `unknown` is **not
an error** — the spooler reported offline and nothing corroborated it; a successful print
clears it.

## 6. Errors you must handle

| Code | Meaning | What the app should do |
|---|---|---|
| `pairing_required` | Not paired, or the pairing was revoked | Run the pairing flow |
| `permission_denied` | The browser blocked local-network access | Show `pb.diagnose().hint`; the user must allow it |
| `agent_absent` | Nothing answered at that host/port/scheme | Tell the user to install or start PrintBridge; check `wss` vs `ws` |
| `not_connected` | The socket closed with a request in flight | Retry; the SDK reconnects automatically |
| `timeout` | No answer within `requestTimeout` | Treat the outcome as unknown and retry with the same `idempotencyKey` |
| `request_failed` | The agent refused the request | Fix the request; the message explains it |

```ts
import { PrintBridgeError } from 'printbridge';

try {
  await pb.printToRole('receipt', escpos);
} catch (err) {
  if (err instanceof PrintBridgeError && err.code === 'pairing_required') {
    await startPairingFlow();
  }
}
```

## 7. Raw WebSocket (without the SDK)

Envelope in, envelope out; `id` correlates a response with its request. Push events carry
no `id`.

```jsonc
// request
{ "version": 1, "id": "req_1", "type": "print",
  "payload": { "role": "kitchen", "data": "<base64>", "job_name": "Order #1234",
               "priority": 0, "idempotency_key": "order-1234" } }

// response
{ "version": 1, "id": "req_1", "type": "response", "success": true,
  "payload": { "job_id": "9f2...", "status": "queued", "created": true, "duplicate": false } }

// push (no id)
{ "version": 1, "type": "job_status_changed", "success": true,
  "payload": { "id": "9f2...", "status": "success" } }
```

Pre-auth types: `ping`, `request_pairing`, `confirm_pairing`, `authenticate`. Everything
else is refused with `{"success":false,"error":"pairing_required"}` until the connection is
paired. The complete type list is in the README protocol table.

Send the token once per connection:

```json
{"version":1,"id":"auth_1","type":"authenticate","payload":{"token":"pb_tok_..."}}
```

## 8. Origin allowlist

Only loopback origins are trusted by default. A POS served from a real hostname must be
allowed:

```json
{"security": {"allowed_origins": ["https://pos.example.com", "http://pos.local:*"]}}
```

or `-allow-origin https://pos.example.com`. A refusal is an HTTP 403 **before** the
WebSocket upgrade and is logged, so "cannot connect" becomes a one-line diagnosis.

## 9. Testing a station end to end

```ts
await pb.testPrint({ role: 'kitchen', slip: 'kitchen' });   // receipt | kitchen | text
const diagnosis = await pb.diagnosePrinter({ role: 'kitchen' });
console.log(diagnosis.checks.join('\n'));
```

`testPrint` builds an ESC/POS slip from the role's capability profile, so it will not send
a cut or bold command the printer cannot handle. If a test slip prints but your own output
does not, the difference is your bytes.

## 10. Operational notes

- **Do not retry blindly on `timeout`** — reuse the `idempotencyKey`.
- The queue refuses new work past `max_depth` (5000) with `queue is full`; surface that to
  the operator instead of retrying in a loop.
- `queue.status()` gives `{paused, counts}` for a "printer status" panel in your UI.
- The agent is a local process: it is not reachable from the network by default and has no
  authentication story for remote callers. For remote printing, use the Cloud Relay
  ([relay.md](relay.md)): the till dials out, the backend posts to the relay with an API key,
  and each job reports back whether it actually printed.
