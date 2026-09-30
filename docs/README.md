# PrintBridge Documentation

| Document | Audience | Read it when |
|---|---|---|
| [install.md](install.md) | On-site IT installer | Deploying to a till, verifying an install, upgrading, or handing over |
| [daily-use.md](daily-use.md) | Installer / site support | Running the dashboard, assigning printers to stations, routine operation |
| [troubleshooting.md](troubleshooting.md) | Installer / site support | Something is broken. Symptom-first. |
| [web-integration.md](web-integration.md) | Developer integrating a web app | Driving printers from your own application, over the SDK or raw WebSocket |
| [integration-walkthrough.md](integration-walkthrough.md) | Developer, first integration | How the logic works end to end, then the integration in 12 steps with a runnable example |
| [relay.md](relay.md) | Developer / ops deploying the cloud tier | Printing to a till the backend cannot reach: deploying the relay, registering agents, the REST API, its security model |
| [architecture.md](architecture.md) | Developer | Understanding the queue, transports, health and security model |
| [sdk_guide.md](sdk_guide.md) | Developer | The SDK API reference |
| [handover.md](handover.md) | Whoever picks up development | What is done, what is deferred, and why |
| [implementation/update-plan.md](implementation/update-plan.md) | Maintainer | The prioritised work plan and the reasoning behind it |

The [top-level README](../README.md) is the feature overview and protocol reference.

---

## Three things that cause most support calls

**1. Know which port you are on.** The default is 9567, and the dashboard is at
`http://localhost:9567/dashboard`. If it was changed (in `config.json` or with `-port`),
resolve it before assuming the agent is broken:

```powershell
Invoke-RestMethod http://localhost:9567/health | Format-List status, port, service
```

`/health` answers with `service = printbridge-agent`, so a responder that *isn't* the
agent is immediately obvious.

**2. Where the state lives.** Console runs keep data in the per-user config directory;
the Windows service keeps it in `%ProgramData%\PrintBridge` because it runs as
LocalSystem.

| Mode | Database and config |
|---|---|
| Windows service | `%ProgramData%\PrintBridge\printbridge.db` and `config.json` |
| Console (`-headless`, `go run .`) | `%AppData%\PrintBridge\printbridge.db` and `config.json` |

**3. A printer role is a station, not a device.** If orders go to the wrong printer,
check the role assignment (`list_printer_roles`, or the dashboard) rather than the
printer's name — a role is what the POS addresses.

```powershell
printbridge-agent.exe -service status    # works without administrator rights
```
