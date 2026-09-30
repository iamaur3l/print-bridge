# Troubleshooting

Audience: on-site support. Symptom-first: find your symptom, read the answer, and only
then go deeper.

Quick facts that resolve most calls:

```powershell
printbridge-agent.exe -service status                                   # is it running?
Invoke-RestMethod http://localhost:9567/health | Format-List *          # what does it think?
Get-Content "$env:ProgramData\PrintBridge\printbridge.log" -Tail 40     # why did it do that?
```

| Symptom | Section |
|---|---|
| Nothing prints, dashboard is fine | [Nothing prints](#nothing-prints) |
| A printer shows `unknown` | [A printer shows unknown](#a-printer-shows-unknown) |
| A printer shows `offline` | [A printer shows offline](#a-printer-shows-offline) |
| "Port USB011 is gone" / printing broke after moving the cable | [The cable was moved](#the-cable-was-moved) |
| Jobs pile up in `failed` | [Jobs are dead-lettered](#jobs-are-dead-lettered) |
| Queue refuses new work | [Queue is full](#queue-is-full) |
| Dashboard or WebSocket does not answer | [Port problems](#port-problems) |
| The web app cannot connect at all | [The web app cannot connect](#the-web-app-cannot-connect) |
| The browser asks about local network access | [Chrome local network access](#chrome-local-network-access) |
| The service will not start | [Service problems](#service-problems) |
| Printers "vanished" from configuration | [Configuration recovery](#configuration-recovery) |

---

## Nothing prints

1. `print` returns a `job_id` even when the printer is offline — the job is queued, not
   printed. Check the job:

   ```json
   {"type":"job_status","payload":{"job_id":"..."}}
   ```

   | Status | Meaning |
   |---|---|
   | `queued` | Waiting, or held because the queue is paused (`queue_status`) |
   | `sending` | Being written right now |
   | `retrying` | A failed attempt with a scheduled retry (`next_attempt_at`) |
   | `success` | Handed to the printer. If nothing came out, it is paper, power or the printer's own lights |
   | `failed` | Dead-lettered after `max_attempts` |

2. `diagnose_printer` gives a plain-language answer for a station or printer:

   ```json
   {"type":"diagnose_printer","payload":{"role":"kitchen"}}
   ```

   Read the `checks` array: it names the transport, the last successful write, the role
   membership, the port binding, and the spooler's verdict.

## A printer shows `unknown`

**This is not a fault.** It means the spooler reported the queue offline and *nothing
corroborated it* — no fault bit, no missing port, no failed probe, no failed write. On
USB thermal printers this happens constantly: Windows sets `WorkOffline` when the device
disappears and frequently never clears it when the device returns.

Print to it. A completed write overrides the flag for two minutes and the state becomes
`online` with the detail `Ready (write verified)`.

## A printer shows `offline`

The detail names the reason. Real causes, in order of likelihood:

| Detail | Cause | Fix |
|---|---|---|
| `Paper out`, `Paper jam`, `Door open`, `Out of toner` | Consumables or a lid | Fix the printer |
| `Paused` | The queue is paused in Windows | `Resume` the printer in Windows |
| `Not reachable` | A network printer that refused the TCP connection | Check power, cable, IP address, and that port 9100 is open |
| `Error`, `User intervention required`, `Not available` | A driver or device fault | Check the printer's own panel; `diagnose_printer` |

## The cable was moved

Every physical USB socket mints a new Windows port (`USB003` → `USB011`), while the print
queue stays pinned to the old one — so printing silently stops. PrintBridge records
where each device-backed queue lives and reports the move:

```json
{"type":"diagnose_printers","payload":{}}
```

```json
{
  "summary": "Kitchen: device VID_04B8&PID_0E15 moved from USB003 to USB011",
  "resolutions": [
    {"printer_name": "Kitchen", "action": "rebind", "current_port": "USB003",
     "new_port": "USB011", "reason": "device VID_04B8&PID_0E15 moved from USB003 to USB011"}
  ]
}
```

Actions you may see: `none` (port present), `rebind` (the device moved), `ambiguous`
(two identical printers are attached — the agent refuses to guess, and you must choose),
`missing` (nothing plausible is connected, or the new port already belongs to another
queue).

**The agent does not move the queue itself yet.** Repoint it in Windows: *Printers &
scanners* → the printer → *Printer properties* → *Ports* → select the new port. Then
confirm with `diagnose_printers` that the action is `none`.

## Jobs are dead-lettered

`failed` means every attempt was used (5 by default). `job_status` carries
`error_message`, which is the printer's own error. Fix the cause, then:

```json
{"type":"retry_job","payload":{"job_id":"..."}}
```

That resets the attempt count and requeues it.

## Queue is full

Past `max_depth` (5000 by default) the agent refuses new work with
`print queue is full (N unfinished jobs)` — deliberately, instead of growing until the
disk fills. It means nothing is draining: check the printers, then either resume the
queue or `clear_failed_jobs`.

## Port problems

Two failure modes look identical from the till: the agent is not running, or something
else owns the port.

```powershell
# Is the agent the one answering?
Invoke-RestMethod http://localhost:9567/health | Select-Object service, port
# Expect: service = printbridge-agent

# What is on the port?
netstat -ano | findstr :9567
```

If `/health` answers with a different `service` value, something else is squatting on
9567 — change the agent's port in `config.json` (`server.port`) or with `-port`, and give
the POS the new port. The agent exits with `failed to start WebSocket server: ... address
already in use` in that situation, which is what the service log will show.

## The web app cannot connect

Work through these in order:

1. **Is the agent running?** `-service status`. If it is running, open
   `http://localhost:9567/dashboard` in the same browser.
2. **Is the origin allowed?** A browser sends an `Origin` header, and only loopback
   origins are trusted by default. A POS served from anywhere else must be listed:

   ```json
   {"security": {"allowed_origins": ["https://pos.example.com"]}}
   ```

   The agent logs the refusal and refuses with HTTP 403 before the WebSocket upgrade:
   `[Server] Rejected WebSocket connection from disallowed origin "https://..."`.
   Origins may use `scheme://host:*` to allow any port on a host.
3. **Is the app paired?** Every request except `ping`, `request_pairing`,
   `confirm_pairing` and `authenticate` requires a token, and an unpaired request answers
   `pairing_required`. Pair again: request a code, read it from the tray/dashboard, and
   confirm it in the app (the code is deliberately never sent to the connecting app).
4. **Is the origin still the same?** A token is only valid for the origin it was issued
   to. Moving the POS to a different hostname invalidates it — revoke the old pairing in
   the dashboard and pair again.
5. **Scheme mismatch.** An HTTPS page cannot open a plain `ws://` socket to a public host;
   the agent needs `-tls-cert`/`-tls-key` so the SDK can use `wss://`. The SDK's `secure`
   option defaults to `true` on HTTPS pages, so this is usually what an
   `agent_absent` error with an HTTPS POS means.

## Chrome local network access

Chrome 142+ gates requests to local addresses behind a permission prompt, and Chrome 147+
extends that to **WebSocket** connections. A POS served from a public HTTPS origin will
therefore ask the user for local network access on first connection.

- The user must click **Allow**; the decision is remembered per origin.
- If they blocked it, re-enable it from the address bar → *Site settings* → *Local
  Network* (or *Apps on device* on Chrome 145+).
- Pages served from `localhost`, `127.0.0.1` or a private IP to the same address space are
  exempt, and Firefox and Safari do not enforce it yet.
- For managed fleets, deploy the `LocalNetworkAccessAllowedForUrls` policy listing the POS
  origin so nobody sees the prompt.
- In the app, `pb.diagnose()` returns a `hint` that names this case, and
  `pb.permissions.localNetwork()` reports `granted` / `prompt` / `denied` /
  `unsupported`.

## Service problems

| Symptom | Cause | Fix |
|---|---|---|
| `-service status` → `not installed` | Never installed, or uninstalled | `-service install` (administrator) |
| `Access is denied` when installing | Not elevated | Run the shell as administrator |
| `-service install` fails with "service could not be started" | The binary path moved after installation | `-service install` again from the new path (repair) |
| Service starts and stops immediately | Usually a bad configuration or an unusable database | Read `%ProgramData%\PrintBridge\printbridge.log` |
| `-service run` from a console fails with "could not talk to the service manager" | Expected: that verb belongs to the SCM | Run the agent without `-service` for console mode |
| Printing stops while logged out | Only possible if something is running the agent per-user | Use the service: an `HKCU\...\Run` entry needs a login |

## Configuration recovery

If settings appear to have reverted, or printers disappeared from configuration:

```powershell
Get-ChildItem "$env:ProgramData\PrintBridge\config.corrupt.*.json"
```

A config file that could not be parsed is copied there **and never overwritten**, so your
settings are recoverable. Fix the JSON in that copy and put it back as `config.json`, then
restart the service. The agent also logs every clamp it applied, for example:

```
[Config] queue.max_concurrent_jobs 90 is excessive; using 32
```

## Escalation: what to capture

When handing a problem on, gather:

```powershell
printbridge-agent.exe -service status
Invoke-RestMethod http://localhost:9567/health | ConvertTo-Json -Depth 5
Get-Content "$env:ProgramData\PrintBridge\printbridge.log" -Tail 200
Get-Printer | Select-Object Name, DriverName, PortName, PrinterStatus
```

That set answers the three questions that matter: is the agent running, does it think it
can print, and does Windows agree about the printers.
