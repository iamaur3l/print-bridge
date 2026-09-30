# Handover Notes

Audience: whoever picks up development. What is done, what is deliberately not, and why.

Start with [implementation/update-plan.md](implementation/update-plan.md) — it carries the
prioritised plan and the reasoning — then [architecture.md](architecture.md) for how the
pieces fit.

---

## Build and test

```powershell
cd agent
go build ./...
go vet ./...
go test ./...                       # 14 packages
go test -bench=. -benchmem ./pkg/queue

cd ../sdk
npm ci; npx tsc --noEmit; npm run build; npm pack --dry-run

# End-to-end smoke test (starts a throwaway agent, exercises the protocol through the
# SDK, and only prints for real if you name a printer)
powershell -ExecutionPolicy Bypass -File agent/scripts/deep_qa.ps1
powershell -ExecutionPolicy Bypass -File agent/scripts/deep_qa.ps1 -Printer "XP-80C"
```

Requirements: Go 1.25+ (the module declares `go 1.25.0`) and Node 22+ for the SDK. The
agent builds without cgo on Windows; the tray (`fyne.io/systray`) needs cgo on macOS and
Linux, which is why release builds for those targets must be produced on their own runners
(see the CI note below).

## What is implemented and verified

| Area | State |
|---|---|
| Raw printing (winspool / CUPS) with ESC/POS, ZPL, EPL passthrough | ✅ verified on Windows; the POSIX path is untested against hardware |
| Corroborated printer status (`online` / `offline` / `unknown`) | ✅ unit-tested; the stale-`WorkOffline` case is the headline regression test |
| Shared cached printer snapshot (4 s TTL, single-flight) | ✅ tested, including concurrent callers |
| `GET /health` verdict + reasons | ✅ tested, including the DNS-rebinding `Host` check |
| TCP 9100 network printers + reachability probes | ✅ the socket path is tested end to end; no real Ethernet printer has been exercised |
| USB port-migration **detection** + `diagnose_printers` | ✅ planner fully tested; the registry walk runs live (0 mappings on the dev machine) |
| Printer roles, capability profiles, ESC/POS test slips | ✅ full authenticated WebSocket flow tested |
| Queue: priority, idempotency, scheduled retries, per-printer parallelism, depth cap, dead-letter, pause/resume/requeue | ✅ nine behaviour tests, including a hung-printer regression test |
| Pairing, origin allowlist, an auth gate on every privileged message, token origin binding, audit log | ✅ tested |
| Browser Local Network Access handling in the SDK | ✅ error classification and `diagnose()`; the Chrome permission itself is documented, not simulated |
| Config file with recovery, clamping, atomic writes | ✅ tested, and exercised live (BOM and corrupt-file cases) |
| Windows service (SCM, dependency, recovery, unelevated status) | ✅ live-verified: `status` works unelevated, and `-service run` fails honestly outside the SCM |
| Auto-update | Opt-in only; the download/apply path is **not** hardware-validated |
| Cloud relay | ✅ hardened and deployable: per-agent secrets (hashed), API key, hibernatable sockets, SQLite DOs, queued jobs, two-stage acks. Deploy steps are verified by `wrangler deploy --dry-run`; the Go side of the protocol is tested. Not yet deployed against a real Cloudflare account |

## Deliberately deferred (and why)

1. **Repointing a print queue after a cable move.** The agent *detects* the move and says
   which port to use; it does not yet write the new port. That needs a
   `GetPrinter`/`SetPrinter` level-2 round trip through a self-referential structure, and it
   mutates a customer's spooler. Validate it on a machine with two USB thermal printers
   (ideally two identical units) before letting it act. The resolver already computes the
   exact target port, so this is a narrow change.
2. **Clearing a stale `WorkOffline` flag.** Same reasoning: detected and reported today,
   written nowhere. Not worth an unvalidated spooler write.
3. **`(Copy 1)` queue-name rebinding.** The port half of "follow the cable" is done; the
   name half is not.
4. **Port fallback + `active_port.txt`.** Deployment pins the port (default 9567) and fails
   loudly if it is taken. A port range plus a discovery file is an improvement, not a
   defect.
5. **Dashboard UI for roles and the queue.** The protocol is complete
   (`list_printer_roles`, `assign_printer_role`, `queue_status`, `pause_queue`, …); the
   embedded dashboard does not expose them yet.
6. **Single-instance lock file.** The SCM prevents duplicate services and a second console
   run exits on the TCP bind; a dedicated lock would produce a cleaner message.
7. **`-Silent` installer switches.** Unnecessary while the service verbs are idempotent.
8. **SDK test suite.** The Go side has 14 tested packages and the relay now has 12
   dependency-free logic tests plus two protocol tests; the SDK still has none
   (`npm test` is a no-op). This is the largest remaining quality gap.
9. **Relay: per-tenant keys and job scoping.** The relay now authenticates both sides
   (per-agent secret hashes in KV, an API key for backends) and fails closed without them.
   What it does not do is distinguish *which* backend may print to *which* till: one shared
   key covers every backend, and any key holder can post to any agent id. Fine for a
   single-tenant tool, insufficient the moment the relay serves two customers.
10. **Relay: rate limiting and per-agent quotas.** A key holder can queue 500 jobs at a
    time and there is no alarm-based retry or backpressure. Storage is capped (24 h, 500
    jobs), which bounds the damage but does not stop a flood.
11. **Auto-update is unvalidated.** It is off by default for exactly this reason: replacing
    the running binary, on Windows, at a till, needs a real test.

## Known debt and risks

| Item | Note |
|---|---|
| ~~CI does not run on `master`~~ | Fixed: `.github/workflows/ci.yml` now triggers on `master` as well |
| ~~CI Go version~~ | Fixed: the agent job reads `go-version-file: agent/go.mod` and also runs `go vet` |
| Relay is not deployed anywhere | Everything up to `wrangler deploy` is verified locally (typecheck, tests, dry-run bundle); no Cloudflare account has been touched yet |
| Release builds | Cross-compiling `darwin-*` and `linux-arm64` from Linux fails because `fyne.io/systray` needs cgo; build them on their own runners, or put the tray behind a build tag |
| Release manifest | `docs/release-notes.md` and `docs/release-manifest.json` are referenced by CI but absent, and `updater.DefaultManifestURL` points at a domain this project does not control |
| Module path | `github.com/printbridge/printbridge/agent` does not match the repository (`iamaur3l/print-bridge`) |
| `gorilla/websocket` | Current, but effectively frozen since 2024; consider `github.com/coder/websocket` |
| TypeScript 7 | A major release exists; upgrade separately and re-check `tsup` |
| `:memory:` stores | Must stay pinned to one connection (`Store.NewStore` does this); a second pooled connection sees an empty database |
| Timestamps | Stored as RFC3339, so second precision; fine for a print queue, but do not add sub-second ordering assumptions |

## Suggested next phases

1. **Validate the deferred spooler work** (items 1–3) on real hardware, then enable it
   behind a flag.
2. **SDK tests**, so a protocol change cannot break a POS silently. The relay's
   `node --test` setup (type stripping, no framework) is a working pattern to copy.
3. **Run the relay against a real Cloudflare account**: `wrangler kv namespace create`,
   `wrangler secret put`, `wrangler deploy`, then a real till. Everything up to the deploy
   is verified; what remains is the account, the KV namespace and hardware.
4. **Relay multi-tenancy** (items 9–10): per-backend keys mapped to allowed agent ids, and
   a rate limit per agent.
5. **Port fallback + `active_port.txt`**, then the dashboard UI for roles and the queue.
6. **Release engineering**: fix the CI triggers and Go version, build per OS, sign the
   installers, and host a real update manifest.
