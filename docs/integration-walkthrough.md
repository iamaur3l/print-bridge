# Integrating PrintBridge into a Website — Step by Step

Audience: a developer adding "Print receipt" to an existing web app.

Two ways to connect, and picking the right one first saves a lot of rework:

| | **Local agent** (this document, steps 1–12) | **Cloud relay** ([relay.md](relay.md)) |
|---|---|---|
| Who talks to the agent | The till's own browser, over `localhost` | Your backend, over HTTPS |
| Page must run on the till | Yes | No — the till only needs internet |
| Setup on the till | Install the agent once | Install the agent + a relay secret |
| Use when | The POS runs in the till's browser | Orders come from a server (online shop, central POS, kitchen display) |

If your website runs in the till's browser, everything below applies as written. If it runs
on a server, skip to [relay.md](relay.md) — the payload is identical, only the transport
changes.

---

# Part 1 — How the logic works

## The five moving parts

```
 (1) Browser page          your website, loaded in the till's browser
      | ws://localhost:9567/ws
 (2) PrintBridge agent     a small tray/service process on the till (Go)
      | the OS spooler, or a TCP 9100 socket
 (3) Printer driver / network printer
      |
 (4) The printer           paper comes out here
 (5) Optional: Cloud relay lets a *server* print to the till over the internet
```

The browser never talks to the printer. It cannot: browsers have no USB or Bluetooth printer
access, and even for a network printer a page cannot open a TCP socket. So the agent is a
**local** process that owns the hardware, and the page talks to it over a WebSocket on
`localhost`. Everything else in the design follows from that split:

- The agent must be **installed on that machine** (like a printer driver).
- The page can only reach it **from that machine** (loopback), by design — the agent is not
  a network service.
- A human must **authorise the page once** (pairing), because otherwise any website could
  print on your printer.

## What happens when you click Print

```
page                      agent                              printer
 |  connect ws://localhost:9567/ws
 |----------------------------->  accept; not paired yet
 |  {type:"print", payload:{role:"receipt", data:"<base64>",
 |                          idempotency_key:"order-1042"}}
 |----------------------------->  (1) pairing/authorisation gate
 |                                (2) resolve the role to a printer name
 |                                (3) INSERT the job into SQLite, status=queued
 |  {id:"sdk_7", success:true, payload:{job_id:"9f2c", status:"queued", duplicate:false}}
 |<----------------------------  returns as soon as it is STORED, not when printed
 |                                (4) worker picks it up: status=sending
 |                                (5) write bytes to the printer ------>  paper
 |                                (6) status=success
 |  {type:"job_status_changed", payload:{id:"9f2c", status:"success"}}
 |<----------------------------  (a push event: no id, so it is not a response)
```

Three things about that sequence are the whole point.

**The command returns before printing happens.** Printing is slow (a thermal printer takes a
second) and can fail (out of paper). If `print` waited for paper, your checkout button would
hang whenever a printer jammed. So the agent stores the job and answers immediately.
Everything after that is asynchronous, and you observe it through events or by polling
`jobs.status(jobId)`.

**A job that is accepted is never lost.** It is committed to disk before the response is
sent, so restarting the agent or the till (or a power cut) replays it. On startup the agent
requeues anything it finds mid-flight.

**`idempotency_key` makes your retry safe.** If your request times out and you retry with the
same key, the agent returns the *original* job (`duplicate: true`) instead of printing a
second receipt. Without it, every retry is another receipt — the most common way to
double-print a customer's order.

## Addresses: a device name, or a station role

```ts
await pb.print('XP-80C', bytes);         // exactly the name the OS knows
await pb.printToRole('receipt', bytes);  // whatever printer currently serves "receipt"
```

Use roles. A device name is a physical fact ("the USB one on the left"); a role is a business
fact ("where receipts come out"). When a printer dies and the shop plugs in a new one, the
role is reassigned on the dashboard and **your code does not change**. The role is resolved,
and the resolved device name recorded, at enqueue time.

## Pairing: why a human has to be involved

Any page in the till's browser can reach `localhost:9567`. Without a gate, any website you
happened to visit could print.

```
page  --request_pairing--------------->  agent generates a 6-digit code (valid 60s)
      <-- {code_requested:true}          the code goes to the TRAY and the DASHBOARD,
                                         never into this response
human reads the code from the tray and types it in
page  --confirm_pairing {code}-------->  agent returns a token
      <-- {token:"pb_tok_..."}           the SDK stores it in localStorage
```

The code is deliberately **not** returned over the socket: if it were, a page could pair
itself and the gate would be decorative. It is a one-time, out-of-band, human-authorised
handshake, like pairing a Bluetooth device.

The token is bound to the **origin** it was issued to: a token issued to
`https://pos.example.com` is worthless to `https://evil.example`. Moving your app to a new
hostname means pairing again, and revoking the app on the dashboard kills the token
immediately.

## Printer status is opinionated (`online` / `offline` / `unknown`)

Windows sets a `WorkOffline` flag on a print queue that **stays set** after the printer comes
back, so the naive "read the spooler flag" approach reports a working printer as offline.
PrintBridge corroborates the verdict and reports `online`, `offline`, or — when the spooler
claims offline but nothing else supports it — **`unknown`**, with `stale_work_offline: true`
and a `status_detail` saying what was actually observed.

Show `unknown` as "unconfirmed", never as "broken". Crying wolf for a stale flag teaches
staff to ignore your status display, which is worse than not having one.

## The queue

Jobs are ordered by `priority` (higher first) then arrival, dispatched in parallel per
printer, retried with backoff up to `max_attempts`, and dead-lettered with the printer's own
error message when attempts run out. One stuck printer does not block the others — the
head-of-line blocking case (a hung printer stalling every other station) has its own
regression test.

---

# Part 2 — Step by step

## Step 0 — Decide who talks to the agent

- Page runs **on the till** (a POS in the till's browser) → local agent, continue here.
- Page runs **on your server** (an online shop) → [relay.md](relay.md).

This decision determines everything else, so make it first. A page on the till cannot use the
relay without a round trip through your server, and a server cannot reach the agent at all.

## Step 1 — Install and start the agent on the till

Follow [install.md](install.md). The short version:

```powershell
# Run it in the foreground once, to watch it start
printbridge-agent.exe -headless
# Then install it as a service so it survives a reboot
printbridge-agent.exe -service install
printbridge-agent.exe -service start
```

Confirm it is ours and it is listening:

```powershell
Invoke-RestMethod http://localhost:9567/health | Select-Object service, status, port
# service=printbridge-agent  status=healthy  port=9567
```

`/health` needs no pairing, so it is the right first call when a till misbehaves.

## Step 2 — Add the SDK to your site

```bash
npm install printbridge
```

```ts
import PrintBridgeClient, { PrintBridgeError } from 'printbridge';
```

Or with a script tag, if your POS is a plain HTML/JS page — the global build works straight
from a `file://` URL too, which is handy for a quick test on a till:

```html
<script src="/vendor/index.global.js"></script>
<script>
  const pb = new PrintBridge.PrintBridgeClient({ host: 'localhost', port: 9567 });
</script>
```

Build it from `sdk/` if you are vendoring it: `npm ci && npm run build` produces
`dist/index.global.js` (use this in a `<script>` tag), `dist/index.mjs` (ESM) and
`dist/index.js` (CommonJS, which is what a Node script would require).

## Step 3 — Create the client

```ts
const pb = new PrintBridgeClient({
  host: 'localhost',
  port: 9567,
  // secure: true on an HTTPS page. The default already picks wss:// for you when
  // location.protocol is https:, so only set this to false deliberately.
  requestTimeout: 15000
});
```

| Option | Default | Notes |
|---|---|---|
| `host` / `port` | `localhost` / `9567` | Where the agent listens |
| `autoConnect` | `true` | Connects on construction; set `false` if you want to control it |
| `secure` | `true` on an HTTPS page | `wss://` instead of `ws://`. **An HTTPS page must use `wss://`** (see step 12) |
| `token` | `localStorage` | Loaded automatically; you rarely pass it |
| `requestTimeout` | `15000` | Rejects a request that is never answered |
| `reconnectInterval` / `maxReconnectDelay` | `3000` / `30000` | Automatic reconnect with backoff — you do not write a reconnect loop |

## Step 4 — Check the agent is there, before anything else

The most common integration bug is a button that does nothing because the agent is not
running, not installed, or the browser blocked access. All three look identical as a bare
failed WebSocket, so ask:

```ts
pb.on('connected', () => setStatus('ready'));
pb.on('disconnected', () => setStatus('offline'));
pb.on('error', (err) => console.warn('PrintBridge:', err.code, err.message));

await pb.connect().catch(() => {});         // never throws into your render path

const diag = await pb.diagnose();
if (!diag.connected) {
  showBanner(diag.hint);                     // a sentence for whoever is at the till
  console.log(diag.url, diag.localNetworkPermission, diag.hint);
}
```

`diagnose()` returns the URL it tried, whether it connected, whether it is using `wss`, the
browser's Local Network Access permission, and a plain-language `hint` such as:

- *"Is the PrintBridge Agent running? Check the tray icon or open
  http://localhost:9567/dashboard."*
- *"Local Network Access is blocked for this origin. Re-enable it from the address bar…"*
- *"This client is using wss://. Start the agent with -tls-cert/-tls-key…"*

Show that hint. It turns a support call into a fix the cashier can make.

## Step 5 — Pair once per origin

```ts
async function ensurePaired(): Promise<boolean> {
  if (pb.pairing.isPaired()) return true;

  const { code_requested } = await pb.pairing.request('Acme POS');
  if (!code_requested) return false;

  const code = prompt('Enter the 6-digit code shown in the PrintBridge tray icon');
  if (!code) return false;

  await pb.pairing.confirm(code.trim());       // the token is stored automatically
  return true;
}
```

The code is in the tray icon and on `http://localhost:9567/dashboard` — not in the response,
by design (Part 1). If your POS is a kiosk with no tray, the dashboard is the channel, so
make sure the operator can open it.

Pairing is **per origin**, so a site served from `https://pos.acme.com` pairs once for that
hostname. The token survives reloads (`localStorage`) and is re-sent automatically on every
reconnect — you do not manage it.

Do not call `request_pairing` in a loop: each call mints a new code and the old one dies,
which is confusing for whoever is reading the tray.

## Step 6 — Check the printers before you promise a receipt

```ts
// Which roles exist, and what they resolve to right now
const roles = await pb.roles.list();
roles.forEach((r) => console.log(r.role, '->', r.printer_name, r.resolved, r.state, r.detail));

// The raw device list, with the corroborated verdict
const printers = await pb.printers.find();
printers.forEach((p) => console.log(p.name, p.type, p.state, p.status_detail));
```

`p.resolved === false` means a role points at a printer that is no longer installed — show
that to the operator instead of printing into the void. `p.state` is `online` / `offline` /
`unknown` (see Part 1); only `offline` should look broken.

To prove a station end to end, print a built-in slip. It is built from the role's capability
profile, so it never sends a cut or bold command the printer cannot handle:

```ts
await pb.testPrint({ role: 'receipt', slip: 'receipt' });   // 'receipt' | 'kitchen' | 'text'
```

If the test slip prints and yours does not, the difference is your bytes — not the plumbing.
That single fact saves hours.

## Step 7 — Build the payload

Two kinds of payload, and mixing them up is the second most common integration bug:

**Raw bytes** — you control every byte, for ESC/POS, ZPL, EPL:

```ts
const escpos = new Uint8Array([
  0x1b, 0x40,                       // ESC @  initialise
  0x1b, 0x61, 0x01,                 // ESC a 1  centre
  0x1b, 0x45, 0x01,                 // ESC E 1  bold on
  ...new TextEncoder().encode('ACME STORE\n'),
  0x1b, 0x45, 0x00,                 // bold off
  ...new TextEncoder().encode('Order 1042\n\n\n'),
  0x1d, 0x56, 0x41, 0x00            // GS V A 0  full cut
]);
await pb.printToRole('receipt', escpos, { jobName: 'Order 1042' });
```

**A string** — convenience for plain text. The SDK UTF-8 encodes it for you (it does not go
through the deprecated `unescape` path, so non-ASCII product names survive):

```ts
await pb.printToRole('receipt', 'ACME STORE\nOrder 1042\n\n\n');
```

Build receipts with a small helper rather than scattering bytes through your business logic:

```ts
const ESC = 0x1b, GS = 0x1d;

function receipt(lines: string[]): Uint8Array {
  const out: number[] = [ESC, 0x40];                       // init
  out.push(ESC, 0x61, 0x01);                               // centre
  out.push(ESC, 0x45, 0x01, ...new TextEncoder().encode('ACME STORE\n'), ESC, 0x45, 0x00);
  out.push(ESC, 0x61, 0x00);                               // left
  for (const line of lines) out.push(...new TextEncoder().encode(line + '\n'));
  out.push(0x0a, 0x0a, 0x0a, GS, 0x56, 0x41, 0x00);        // feed, then cut
  return new Uint8Array(out);
}
```

Two rules worth keeping:

- **Use 32-column line widths on a 58 mm printer, 42–48 on 80 mm.** Text that wraps
  unpredictably reads as a broken receipt. The role's capability profile reports
  `max_width` if you want to be exact: `(await pb.roles.list()).find(r => r.role === 'receipt')?.capabilities.max_width`.
- **A cut command the printer does not support leaves the paper hanging.** That is what
  `capabilities.supports_cut` is for, and why `testPrint` exists.

## Step 8 — Print, safely

```ts
try {
  const { jobId, duplicate } = await pb.printToRole('receipt', receipt(ORDER_LINES), {
    jobName: `Order ${order.id}`,
    idempotencyKey: `order-${order.id}`,   // safe to retry, never double-prints
    priority: 0                            // raise to push ahead of queued jobs
  });
  console.log(duplicate ? 'already queued' : 'queued', jobId);
  followJob(jobId);
} catch (err) {
  handlePrintError(err, order);
}
```

What the return value means: the job is **stored**, not printed. A cashier-friendly UI says
"Sent to printer, 1 job queued", not "Printed".

For a kitchen ticket that must jump the queue:

```ts
await pb.printToRole('kitchen', ticketBytes, { priority: 10, jobName: `Ticket ${order.id}` });
```

## Step 9 — Follow the outcome

```ts
pb.on('job_status_changed', ({ id, status, error_message }) => {
  // status: queued | sending | success | failed | retrying
  if (status === 'success') markPrinted(id);
  if (status === 'failed')  showAlert(`Printer problem: ${error_message}`);
});

function followJob(jobId: string) {
  pb.jobs.status(jobId).then(console.log);          // queued | sending | success | failed | retrying
}
```

Events are the primary channel; `jobs.status(jobId)` is for a one-off check or a poll after a
reload. A job that ends `failed` has been **dead-lettered** with the printer's own error
(usually a wrong printer name or a printer that is switched off), and can be re-run from the
dashboard or with `pb.queue.retryJob(jobId)`.

A status panel for the till operator, in two calls:

```ts
const { paused, counts } = await pb.queue.status();
// counts: { queued, sending, success, failed, retrying, total }
if (counts.failed > 0) showAlert(`${counts.failed} job(s) could not print — open the dashboard`);
```

## Step 10 — Handle the errors you will actually see

```ts
import { PrintBridgeError } from 'printbridge';

async function handlePrintError(err: unknown, order: Order) {
  if (!(err instanceof PrintBridgeError)) throw err;

  switch (err.code) {
    case 'pairing_required':                      // not paired, or revoked
      if (await ensurePaired()) retry(order);     // same idempotencyKey, so no duplicate
      break;
    case 'timeout':                               // unknown outcome: the job may have printed
      retry(order);                               // ALWAYS with the same idempotencyKey
      break;
    case 'not_connected':                         // socket dropped mid-request; SDK reconnects
      retry(order);
      break;
    case 'agent_absent':                          // not installed, not running, or wrong scheme
      showBanner('PrintBridge is not running on this till');
      break;
    case 'permission_denied':                     // the browser blocked local network access
      showBanner((await pb.diagnose()).hint);
      break;
    case 'request_failed':                        // the agent refused it; the message says why
      showAlert(err.message);
      break;
  }
}
```

Rules that keep tills out of trouble:

- **Never retry without the same `idempotencyKey`.** A duplicate key returns the original
  job; a missing key prints a second receipt.
- **Never retry `request_failed` unchanged** — the agent already explained itself (unknown
  printer, `queue is full`, invalid base64).
- **Do not block the checkout UI on a print.** Queue it, tell the cashier it is queued, and
  let the event stream report failure.

## Step 11 — Allow your origin, if the POS is not on localhost

Only loopback origins are trusted by default, so a POS served from a real hostname is
refused with an **HTTP 403 before the WebSocket upgrade** (and the refusal is logged, so
"cannot connect" becomes one line of diagnosis rather than a hunt).

```json
{ "security": { "allowed_origins": ["https://pos.acme.com", "http://till-*.acme.local:*"] } }
```

or on the command line:

```powershell
printbridge-agent.exe -allow-origin https://pos.acme.com -allow-origin https://pos-staging.acme.com
```

Restart the agent after changing it, then reload the page. If you see a 403 in the browser's
network log, this is the cause — not pairing.

## Step 12 — HTTPS, wss and the browser's own gate

Two independent things can block a connection that "should" work:

**Mixed content.** A page served over `https://` cannot open a plain `ws://` socket. The SDK
defaults to `wss://` on an HTTPS page, which means the *agent* must speak TLS:

```powershell
printbridge-agent.exe -tls-cert C:\PrintBridge\cert.pem -tls-key C:\PrintBridge\key.pem
```

Use a certificate the till's browser already trusts (or one you install into the till's trust
store). During development, keeping the POS on `http://localhost` avoids this entirely.

**Chrome's Local Network Access.** Chrome 142+ gates requests to the local network, and
Chrome 147+ extends that to WebSockets. The first attempt shows a permission prompt; if it
was ever denied, the page gets nothing and the console shows a blocked request. The SDK
reports this as `permission_denied` and `diagnose()` explains how to re-enable it
(Site settings → Local Network, "Apps on device" on some versions). Firefox and Safari do not
enforce it yet.

---

# Part 3 — A complete, runnable example

`examples/checkout-print.html` is a single self-contained page that does everything above:
connects, diagnoses a failure, pairs, lists roles and printers, prints an ESC/POS receipt to
a role with an idempotency key, follows the job, opens the cash drawer, and shows the queue.

Run it on a till:

```powershell
# 1. Build the SDK (once)
cd sdk; npm ci; npm run build

# 2. Serve the repository over HTTP (a file:// page can also load the global build)
cd ..; python -m http.server 8080

# 3. On the till, open http://localhost:8080/examples/checkout-print.html
```

It is deliberately noisy: every step logs what it sent and what came back, so the browser
console doubles as the integration checklist. Because a browser cannot be driven from a
terminal, the same protocol path is covered end to end by a Node script, `deep_qa.mjs`, which
drives the built SDK against a throwaway agent:

```powershell
powershell -ExecutionPolicy Bypass -File agent/scripts/deep_qa.ps1
```

If `deep_qa` passes and your page does not, the problem is in the page or the browser's
permissions — not in the agent.

---

# Part 4 — Checklist and the mistakes that actually happen

## Before you ship

- [ ] The agent is installed as a **service** on every till, so it survives a reboot
- [ ] A role (`receipt`, `kitchen`) is assigned on every till, not a device name in your code
- [ ] Every print uses an `idempotencyKey` derived from the order id
- [ ] The UI says "queued", and the outcome arrives through `job_status_changed`
- [ ] `diagnose().hint` is shown when the connection fails, not a generic "print failed"
- [ ] The POS origin is in `allowed_origins`, and the agent was restarted after the change
- [ ] An HTTPS POS has an agent with `-tls-cert`/`-tls-key` (or the POS is on loopback)
- [ ] `pb.testPrint({ role: 'receipt', slip: 'receipt' })` prints on every till
- [ ] A failed job is visible to staff (queue panel or dashboard link), not just a console log

## Symptom → cause

| Symptom | Almost always |
|---|---|
| `403` in the network log, no WebSocket | The POS origin is not in `allowed_origins` |
| Button does nothing, no error | Agent not running; call `diagnose()` and show the hint |
| `pairing_required` on every print | Never paired, or the app was revoked on the dashboard |
| Worked yesterday, not today, new hostname | Pairing is per origin — pair again |
| `agent_absent` on an HTTPS page | Page is `https://`, socket is `ws://` — start the agent with TLS |
| Blocked request, "local network access" | Chrome LNA permission denied — re-enable via the address bar |
| Prints garbage characters | Wrong payload: plain text sent to a printer expecting ESC/POS, or vice versa |
| Two receipts for one order | Retry without an `idempotencyKey` |
| Receipt printed but the UI says failed | You waited for `success` inside the same request; observe it via events |
| `request_failed: The printer name is invalid` | The name or role does not exist on that till — check `printers.find()` |
| Job stuck in `queued` | Queue paused (`queue.status().paused`) or no printer is available |

## If your site runs on a server

Everything above changes in exactly one way: your **backend** posts the same payload to the
relay instead of the browser posting it to `localhost`, and the outcome comes back as
`accepted` / `completed` on the job you polled. See [relay.md](relay.md) — including how to
register the till's secret and the two-stage acknowledgement that tells you whether the
receipt actually printed.




