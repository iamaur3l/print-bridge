# Installation & Verification

Audience: the person deploying PrintBridge to a till. Assumes administrator rights for
installation and familiarity with Windows services.

Day-to-day operation is in [daily-use.md](daily-use.md); when something is broken see
[troubleshooting.md](troubleshooting.md); to connect a web app see
[web-integration.md](web-integration.md).

---

## What gets installed

| Path | Contents |
|---|---|
| `%ProgramFiles%\PrintBridge\printbridge-agent.exe` | The agent (a single static binary, no runtime to install) |
| `%ProgramData%\PrintBridge\printbridge.db` | Job queue, pairings, audit log, roles, port bindings |
| `%ProgramData%\PrintBridge\config.json` | Settings; created with defaults on first run |
| `%ProgramData%\PrintBridge\config.corrupt.<timestamp>.json` | A copy of any config file that could not be parsed |
| Windows service `printbridge-agent` | Display name **PrintBridge Agent** |

The service runs as `LocalSystem`, starts automatically, depends on the Print Spooler,
and is configured to restart itself 5 s, 10 s and 30 s after a failure.

## Prerequisites

| Requirement | Notes |
|---|---|
| Windows 10/11 or Server 2016+ | x64 or arm64 |
| Administrator rights | To register the service. `-service status` needs no elevation. |
| Print Spooler running | The service declares it as a dependency, so Windows starts it first |
| Printer driver installed | Windows must be able to see the printer (`Get-Printer`). Network printers on a TCP port do **not** need a driver — see below. |

## Install

### Option A — installer

Run `PrintBridgeAgentSetup.exe` as administrator. It copies the binary, creates
`%ProgramData%\PrintBridge`, registers the service, and offers to start it.

### Option B — register the service yourself

```powershell
printbridge-agent.exe -service install     # needs administrator rights
printbridge-agent.exe -service start
```

`-service install` doubles as **repair**: an existing registration is replaced and
`config.json` is never touched. Use it for a half-finished or failed install rather
than editing the service by hand.

### Option C — console mode (workstation testing, no service)

```powershell
printbridge-agent.exe -headless            # Ctrl+C to stop
printbridge-agent.exe                     # with the tray icon
```

Console mode reads and writes `%AppData%\PrintBridge`, so it never disturbs a service
installation.

## Verify the install

Run all five. Any failure sends you to [troubleshooting.md](troubleshooting.md).

```powershell
# 1. The service exists and is running
printbridge-agent.exe -service status

# 2. It is really our agent (the service field proves it), and it says what it thinks
Invoke-RestMethod http://localhost:9567/health | Format-List status, reasons, port, uptime_seconds

# 3. The dashboard renders
Start-Process http://localhost:9567/dashboard

# 4. Queue depth and pause state
#    From the dashboard, or with a paired client (see web-integration.md)

# 5. A real print reaches a real printer
#    Dashboard -> Printers -> Test print
#    Or over the WebSocket: {"type":"test_print","payload":{"printer":"<name>","slip":"receipt"}}
```

`/health` answers `healthy`, `degraded` or `unhealthy` with a plain-language `reasons`
list:

| Status | Meaning | Action |
|---|---|---|
| `healthy` | Has a usable printer | None |
| `degraded` | Running, but cannot currently complete work | Read `reasons` — usually a printer that is offline, or one whose status could not be verified |
| `unhealthy` | Cannot accept work at all (HTTP 503) | No printers discovered, or the store is unusable |

## First printer setup

1. Open the dashboard: `http://localhost:9567/dashboard`.
2. Confirm the printers are listed. A printer shown as **`unknown`** is not broken —
   read `status_detail`: the spooler reported offline but nothing corroborated it (see
   [troubleshooting.md](troubleshooting.md#a-printer-shows-unknown)).
3. Assign stations to printers, so the POS addresses roles rather than devices:

   ```json
   {"type":"assign_printer_role","payload":{"role":"receipt","printer":"XP-80C","label":"Front counter"}}
   {"type":"assign_printer_role","payload":{"role":"kitchen","printer":"XP-80C @ Kitchen","capabilities":{"max_width":32}}}
   ```

   Roles are what the POS asks for. **Replacing a printer is then a role reassignment
   and needs no change in the POS.**
4. Send a test print per role (`test_print` with `slip: "receipt"` or `"kitchen"`).
5. Fire the cash drawer if one is fitted. `test_print` does not kick the drawer; use
   `open_drawer` with `pin: 2` and then `pin: 5` to find the correct wiring.

## Network (Ethernet) printers

A queue whose Windows port is a TCP address — `IP_192.168.1.100`, `10.0.0.5:9100`,
`printer.local` — is written to **directly** over its RAW/JetDirect port. Windows needs
no printer driver for it, and the agent probes it with a real TCP connection. Check it
with:

```powershell
printbridge-agent.exe -net 192.168.1.100 -probe      # is the printer there at all?
printbridge-agent.exe -net 192.168.1.100 -escpos     # send a test slip directly
```

Device and virtual ports (`USB001`, `LPT1`, `DOT4_001`, `PORTPROMPT:`, `WSD-…`) keep
using the OS spooler, so nothing changes for existing installations.

## Updating an install

```powershell
printbridge-agent.exe -service stop
# replace printbridge-agent.exe with the new build
printbridge-agent.exe -service install     # repair: re-registers, keeps config.json
printbridge-agent.exe -service start
```

The agent does **not** update itself unless auto-update is explicitly enabled
(`-auto-update`, or `"updater": { "auto_update": true }`). Replacing the running binary
is not something an unattended till should do by surprise.

## Uninstall

```powershell
printbridge-agent.exe -service uninstall
```

The service is removed; `%ProgramData%\PrintBridge` is deliberately left in place, so
delete it by hand for a clean slate.

## Handover checklist

- [ ] `-service status` reports the service running
- [ ] `/health` returns `healthy` with `service: printbridge-agent`
- [ ] Every station role resolves to the intended printer (`list_printer_roles`)
- [ ] A test receipt printed from the `receipt` role
- [ ] A test ticket printed from each kitchen-style role
- [ ] Cash drawer verified, if fitted
- [ ] The POS origin is listed in `config.json` under `security.allowed_origins` (or was
      added with `-allow-origin`); a browser client from another origin is refused
- [ ] Pairing is done and the POS stores the token
- [ ] The machine was **actually rebooted** and printing still works afterwards
- [ ] Database and config paths are recorded on the handover sheet
