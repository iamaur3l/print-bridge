# PrintBridge SDK Developer Guide

The `printbridge.js` SDK connects browser-based web applications to the local PrintBridge Agent via WebSocket.

## Installation

```bash
npm install printbridge.js
```

For plain HTML pages without a bundler, use the UMD build:

```html
<script src="node_modules/printbridge.js/dist/index.global.js"></script>
<script>
  const pb = new PrintBridgeClient();
</script>
```

## Quick Start

```typescript
import PrintBridgeClient from 'printbridge.js';

const pb = new PrintBridgeClient({
  host: 'localhost',
  port: 9567,
  autoConnect: true
});

// 1. Pair with the agent
const pairResp = await pb.pairing.request('My POS App');
// User sees a 6-digit code in the tray/dashboard
const confirmResp = await pb.pairing.confirm('123456');
console.log('Paired! Token:', confirmResp.token);

// 2. List printers
const printers = await pb.printers.find();
console.log('Available printers:', printers);

// 3. Print raw data (ESC/POS, ZPL, EPL)
const escposData = new Uint8Array([0x1B, 0x40, 0x48, 0x65, 0x6C, 0x6C, 0x6F, 0x0A, 0x1D, 0x56, 0x41, 0x00]);
const result = await pb.print('ReceiptPrinter', escposData, { jobName: 'Order #1234' });
console.log('Job ID:', result.jobId);

// 4. Monitor job status
pb.on('job_status_changed', (job) => {
  console.log(`Job ${job.id}: ${job.status}`);
});

// 5. Monitor printer health
pb.on('printer_status_changed', (event) => {
  console.log(`Printer ${event.printer.printer_name}: ${event.printer.is_online ? 'online' : 'offline'}`);
});
```

## API Reference

### `PrintBridgeClient(config)`

| Option | Default | Description |
|--------|---------|-------------|
| `host` | `'localhost'` | Agent hostname |
| `port` | `9567` | Agent WebSocket port |
| `autoConnect` | `true` | Connect on construction |
| `token` | `localStorage` | Saved auth token |
| `reconnectInterval` | `3000` | Initial reconnect delay (ms), doubling up to `maxReconnectDelay` |
| `maxReconnectDelay` | `30000` | Upper bound for reconnect backoff (ms) |
| `requestTimeout` | `15000` | Reject a request that gets no response after this many ms |
| `secure` | `true` on HTTPS pages | Use `wss://` instead of `ws://` |

### Methods

| Method | Returns | Description |
|--------|---------|-------------|
| `connect()` | `Promise<void>` | Open WebSocket connection |
| `disconnect()` | `void` | Close connection |
| `print(printer, data, options?)` | `Promise<PrintResult>` | Enqueue a print job (`priority`, `idempotencyKey`, `role`) |
| `queue.status()` | `Promise<QueueStatus>` | Queue depth by status and the pause state |
| `queue.pause()` / `queue.resume()` | `Promise` | Hold or resume dispatch without dropping work |
| `queue.retryJob(jobId)` | `Promise` | Requeue a dead-letter job with a clean attempt count |
| `queue.clearFailed()` | `Promise` | Discard dead-letter jobs |
| `printToRole(role, data, options?)` | `Promise<{jobId}>` | Enqueue a job addressed to a station role |
| `roles.assign(role, printer, options?)` | `Promise` | Point a station at a printer (with optional capabilities) |
| `roles.list()` | `Promise<PrinterRoleStatus[]>` | Roles with their live resolution |
| `roles.remove(role)` | `Promise` | Remove a station |
| `testPrint({ printer \| role, slip? })` | `Promise` | Send a built-in ESC/POS test slip |
| `diagnosePrinter({ printer \| role })` | `Promise<PrinterDiagnosis>` | Plain-language diagnosis with `checks[]` |
| `authenticate(token?)` | `Promise` | Authenticate with saved or given token |
| `pairing.request(appName)` | `Promise<{code_requested, expires_at}>` | Request a pairing code (the code itself appears in the agent tray/dashboard) |
| `pairing.confirm(code)` | `Promise<{token}>` | Confirm pairing with code |
| `pairing.setToken(token)` | `void` | Persist token to localStorage |
| `pairing.getToken()` | `string` | Get current token |
| `pairing.isPaired()` | `boolean` | Check if token exists |
| `permissions.localNetwork()` | `Promise<'granted'\|'prompt'\|'denied'\|'unsupported'>` | Query Chrome's Local Network Access permission |
| `diagnose()` | `Promise<{url, connected, secure, localNetworkPermission, hint}>` | Explain why a connection is failing |
| `connected` (getter) | `boolean` | Current connection state |
| `url` (getter) | `string` | The WebSocket URL in use |
| `printers.find()` | `Promise<PrinterInfo[]>` | List system printers |
| `printers.health()` | `Promise<PrinterHealth[]>` | Get printer status |
| `jobs.status(jobId)` | `Promise<JobStatus>` | Query job status |
| `devices.openDrawer(printer, opts?)` | `Promise` | Kick cash drawer |
| `devices.listSerialPorts()` | `Promise<SerialPort[]>` | List serial ports |
| `devices.listHIDDevices()` | `Promise<HIDDevice[]>` | List HID devices |
| `on(event, callback)` | `void` | Subscribe to events |
| `off(event, callback)` | `void` | Unsubscribe from events |

### Events

| Event | Payload | Description |
|-------|---------|-------------|
| `connected` | `{host, port}` | WebSocket opened |
| `disconnected` | `{}` | WebSocket closed |
| `error` | `PrintBridgeError` (has a `code`) | Connection error |
| `job_status_changed` | `Job` | Job status update |
| `printer_status_changed` | `{printer, timestamp}` | Printer health change |

## TypeScript / React Example

```tsx
import { useEffect, useState } from 'react';
import PrintBridgeClient from 'printbridge.js';

function PrintButton() {
  const [pb] = useState(() => new PrintBridgeClient({ autoConnect: true }));
  const [status, setStatus] = useState('disconnected');

  useEffect(() => {
    pb.on('connected', () => setStatus('connected'));
    pb.on('disconnected', () => setStatus('disconnected'));
    return () => pb.disconnect();
  }, []);

  const handlePrint = async () => {
    if (!pb.pairing.isPaired()) {
      await pb.pairing.request('React POS');
      const code = prompt('Enter pairing code from tray:');
      if (code) await pb.pairing.confirm(code);
    }
    const data = new Uint8Array([0x1B, 0x40, 0x48, 0x65, 0x6C, 0x6C, 0x6F, 0x0A]);
    await pb.print('ThermalPrinter', data, { jobName: 'Receipt' });
  };

  return (
    <button onClick={handlePrint} disabled={status !== 'connected'}>
      {status === 'connected' ? 'Print Receipt' : 'Connecting...'}
    </button>
  );
}
```

## Vue.js Example

```vue
<template>
  <button @click="printReceipt" :disabled="!connected">
    {{ connected ? 'Print' : 'Connecting...' }}
  </button>
</template>

<script setup>
import { ref, onMounted, onUnmounted } from 'vue';
import PrintBridgeClient from 'printbridge.js';

const pb = new PrintBridgeClient({ autoConnect: true });
const connected = ref(false);

onMounted(() => {
  pb.on('connected', () => connected.value = true);
  pb.on('disconnected', () => connected.value = false);
});

onUnmounted(() => pb.disconnect());

async function printReceipt() {
  const data = new Uint8Array([0x1B, 0x40, 0x48, 0x65, 0x6C, 0x6C, 0x6F, 0x0A]);
  await pb.print('ReceiptPrinter', data, { jobName: 'Vue Order' });
}
</script>
```

## Next.js / SSR

When using Next.js or other SSR frameworks, disable auto-connect on the server:

```typescript
const isBrowser = typeof window !== 'undefined';
const pb = isBrowser ? new PrintBridgeClient({ autoConnect: true }) : null;
```

## Migrating from QZ Tray

| QZ Tray API | PrintBridge Equivalent |
|-------------|----------------------|
| `qz.security.setSignaturePromise()` | `pb.pairing.request()` + `confirm()` |
| `qz.printers.find()` | `pb.printers.find()` |
| `qz.print(config, data)` | `pb.print(printer, data, options)` |
| `qz.websocket.connect()` | `pb.connect()` (auto) |
| `qz.devices.openDrawer()` | `pb.devices.openDrawer()` |
| No equivalent | `pb.jobs.status()` (queue visibility) |
| No equivalent | `pb.on('printer_status_changed')` |

## Error handling

Every rejection is a `PrintBridgeError` carrying a machine-readable `code`:

| Code | Meaning | What to do |
|------|---------|------------|
| `not_connected` | Socket closed, or closed before the response arrived | Start the agent / wait for reconnect |
| `timeout` | No response within `requestTimeout` ms | Check the agent log — the job may still have been accepted |
| `pairing_required` | This connection is not paired | Run the pairing flow |
| `permission_denied` | The browser blocked local-network access | See the Local Network Access section below |
| `agent_absent` | Nothing answered at the configured host/port/scheme | Check the tray icon, the port, and whether the agent has TLS enabled |
| `request_failed` | The agent returned an error, or the payload could not be encoded | Read the message text |

```ts
import PrintBridgeClient, { PrintBridgeError } from 'printbridge';

try {
  await pb.print('ReceiptPrinter', data);
} catch (err) {
  if (err instanceof PrintBridgeError && err.code === 'pairing_required') {
    // start the pairing flow
  }
}
```

## Chrome Local Network Access (read this before shipping)

Chrome 142+ requires user permission for fetch/XHR to local addresses, and **Chrome 147+ extends that permission to WebSocket connections**. A POS UI served from a public HTTPS origin will prompt the user before its first connection to `ws://localhost:9567`. Firefox and Safari do not enforce this yet, and pages served from `localhost`, `127.0.0.1` or a private IP contacting the same address space are exempt.

Give the user a recovery path instead of letting the connection fail silently:

```ts
const report = await pb.diagnose();
if (!report.connected) {
  showBanner(`${report.hint} (${report.url})`);
}

const permission = await pb.permissions.localNetwork();
// 'denied'      -> user must re-enable via the address bar: Site settings -> Local Network
// 'prompt'      -> the next connection will ask; tell the user to click Allow
// 'unsupported' -> not a Chromium browser, or the permission is not implemented yet
```

For managed fleets, deploy the `LocalNetworkAccessAllowedForUrls` enterprise policy (Chrome and Edge) listing your POS origin to pre-grant the permission. If your POS UI runs inside an iframe, add `allow="local-network-access"` (or `local-network; loopback-network`) to the frame.

If your web app is served over HTTPS, the agent must be started with `-tls-cert`/`-tls-key` so the SDK can use `wss://` (the `secure` option defaults to `true` on HTTPS pages).

## Troubleshooting

| Problem | Solution |
|---------|----------|
| `pairing_required` error | Call `pairing.request()` then `pairing.confirm()` with the code shown in the agent tray |
| Connection refused | Ensure PrintBridge Agent is running (`go run main.go` or check system tray) |
| Connection refused only from your POS origin | The origin is not allowlisted — restart the agent with `-allow-origin https://your-pos.example.com` |
| First connection never completes on HTTPS POS | Chrome Local Network Access is blocked — re-enable from the address bar (Site settings → Local Network, or "Apps on device" on Chrome 145+) or pre-grant via policy |
| Token lost after restart | Token is stored in `localStorage` and persists; if cleared, re-pair |
| Print job stuck in `queued` | Check printer is online and not jammed; jobs auto-retry with backoff |
| WebSocket disconnects | SDK auto-reconnects with exponential backoff; call `disconnect()` to stop reconnecting |
| Every call rejects with `agent_absent` | Check the transport: an HTTPS page uses `wss://`, so the agent needs `-tls-cert`/`-tls-key` (or construct the client with `{ secure: false }`) |
| Requests hang with no error | Requests now time out after `requestTimeout` ms (default 15s) and reject with `timeout` |
