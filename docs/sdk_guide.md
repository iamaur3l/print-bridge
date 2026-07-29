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
| `reconnectInterval` | `3000` | Reconnect delay (ms) |

### Methods

| Method | Returns | Description |
|--------|---------|-------------|
| `connect()` | `Promise<void>` | Open WebSocket connection |
| `disconnect()` | `void` | Close connection |
| `print(printer, data, options?)` | `Promise<{jobId}>` | Enqueue a print job |
| `authenticate(token?)` | `Promise` | Authenticate with saved or given token |
| `pairing.request(appName)` | `Promise<{code}>` | Request a pairing code |
| `pairing.confirm(code)` | `Promise<{token}>` | Confirm pairing with code |
| `pairing.setToken(token)` | `void` | Persist token to localStorage |
| `pairing.getToken()` | `string` | Get current token |
| `pairing.isPaired()` | `boolean` | Check if token exists |
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
| `error` | `Error` | Connection error |
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

## Troubleshooting

| Problem | Solution |
|---------|----------|
| `pairing_required` error | Call `pairing.request()` then `pairing.confirm()` with the code shown in the agent tray |
| Connection refused | Ensure PrintBridge Agent is running (`go run main.go` or check system tray) |
| Token lost after restart | Token is stored in `localStorage` and persists; if cleared, re-pair |
| Print job stuck in `queued` | Check printer is online and not jammed; jobs auto-retry with backoff |
| WebSocket disconnects | SDK auto-reconnects with 3-second interval |
