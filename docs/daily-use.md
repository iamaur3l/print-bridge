# Day-to-Day Operation

Audience: whoever supports the site after installation. Covers the dashboard, the
routine tasks, and the operational rules that are not obvious.

See [install.md](install.md) to deploy, [troubleshooting.md](troubleshooting.md) when
something breaks, [web-integration.md](web-integration.md) to connect a web app.

---

## Reaching the dashboard

```
http://localhost:9567/dashboard
```

Always confirm the port rather than assuming it — it can be changed in `config.json` or
with `-port`, and something else may already be on 9567:

```powershell
Invoke-RestMethod http://localhost:9567/health | Format-List status, port, service
```

The dashboard shows the printers with their state, recent jobs, the paired
applications, and the current pairing code. `/health` is safe to poll from monitoring:
it needs no pairing, sets no CORS headers (so a browser page cannot read it), and rejects
requests whose `Host` header is not this machine.

## Printer states

| State | Meaning | Action |
|---|---|---|
| `online` | Usable. The detail says how we know: `Ready`, `Ready (write verified)` (a print completed recently), or `Ready (transport reachable)` (a network printer answered) | None |
| `unknown` | The spooler reported offline, but nothing corroborated it — the classic stale `WorkOffline` flag on a USB thermal printer | None: print to it. A successful print clears the doubt |
| `offline` | A real fault (jam, paper out, door open, no toner, user intervention, error), a paused queue, or an unreachable printer | Fix the hardware, then use **Test print** |

Read `status_detail`: it is the reasoning, not a hint.

## Station roles

Printer ids are **stations, not devices**. The POS asks for `receipt` or `kitchen`; which
physical printer serves that station is configuration.

- **Adding or replacing a printer** → reassign the role. The POS needs no change.
- **Swapping a failed printer mid-shift** → assign the role to another printer, print a
  test slip, carry on.
- **A role that stops resolving** (printer renamed or removed) reports itself in
  `list_printer_roles` with `"resolved": false` and a reason, and using it fails with a
  clear error rather than printing nowhere.
- Deleting a role breaks every call that names it. Reassign instead of deleting.

```json
{"type":"assign_printer_role","payload":{"role":"kitchen","printer":"XP-80C @ Kitchen"}}
{"type":"list_printer_roles","payload":{}}
```

Capability profiles travel with the role and control the built-in test slips, so
`test_print` never sends a cut or bold command to a printer that cannot do it. Use
`"max_width": 32` for 58 mm paper, 48 for 80 mm.

## The queue

The queue is why a till survives a printer dying: every accepted job is stored before
dispatch, so nothing is lost when the printer, the agent or the machine goes away.

| Task | How |
|---|---|
| See the depth | `{"type":"queue_status"}` → `{paused, counts{queued, sending, success, failed, retrying, total}}` |
| Pause while swapping hardware | `{"type":"pause_queue"}` (work is held, not dropped) |
| Resume | `{"type":"resume_queue"}` |
| Re-send a dead-letter job | `{"type":"retry_job","payload":{"job_id":"..."}}` |
| Discard dead-letter jobs | `{"type":"clear_failed_jobs"}` |

How to read the counts:

- **`retrying`** is normal: a failed attempt is scheduled with backoff (1 s, 2 s, 4 s,
  8 s, then a 10 s cap) rather than blocking anything, so other printers keep printing.
- **`failed`** is the dead-letter state: the job exhausted its attempts. It counts
  against `/health`, which reports it by name.
- A **growing `queued` count with an idle printer** means dispatch is blocked, not slow.
  Check the printer status before clearing anything.

## Cash drawer

Use `open_drawer` (not `test_print`):

```json
{"type":"open_drawer","payload":{"printer":"XP-80C","pin":2}}
```

If nothing happens the drawer is almost always on the other pin: try `pin: 5`. Star
Micronics drawers use a different pulse, so pass `"brand":"star"`. This is drawer wiring,
not a software fault.

## Configuration changes

Edit `%ProgramData%\PrintBridge\config.json` (service) or
`%AppData%\PrintBridge\config.json` (console), then restart:

```powershell
printbridge-agent.exe -service stop
printbridge-agent.exe -service start
```

Rules worth knowing:

- A config file that fails to parse is **backed up** as
  `config.corrupt.<timestamp>.json` and defaults are used. If printers appear to
  "vanish", restore from that file.
- Out-of-range values are clamped and each clamp is logged. Nothing is silently ignored.
- Writes are atomic, so a power cut during an edit cannot leave a half-written file.
- There is no settings page in the dashboard yet: configuration is the file plus flags.

## Logs

| Mode | Where |
|---|---|
| Windows service | `%ProgramData%\PrintBridge\printbridge.log` (rotated once to `.log.1` at 5 MB) |
| Console | standard error, so redirect it if you need a file |

Read the service log first: it carries the configuration decisions, port-binding checks,
queue outcomes and the reason the service stopped. Typical useful lines:

```
[Config] config file: C:\ProgramData\PrintBridge\config.json
[Config] <path> is not valid JSON (...); it was backed up to ... and defaults are in use
[Server] WebSocket & Dashboard server listening on http://127.0.0.1:9567
[Server] Port check: 2 device-backed queue(s) tracked, 3 live device mapping(s) on this machine
[Queue] Job 1f2c... printed successfully to "XP-80C" (attempts: 1)
[Queue] Job 1f2c... dead-lettered after 5 attempts
[Printer] Direct TCP write to 192.168.1.100:9100 failed (...); falling back to the spooler queue "..."
```

## Weekly sanity check

```powershell
printbridge-agent.exe -service status
$h = Invoke-RestMethod http://localhost:9567/health
"status : $($h.status)"
"reasons: $($h.reasons -join '; ')"
"queue  : queued=$($h.queue.queued) retrying=$($h.queue.retrying) failed=$($h.queue.failed)"
$h.printers | Format-Table name, state, status_description, type
```

A `degraded` status whose reason names a printer is the early warning that a printer is
unplugged or out of paper — worth acting on before the lunch rush rather than during it.
