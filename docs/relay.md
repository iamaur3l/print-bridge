# Cloud Relay

The relay lets a backend print on a till that it cannot reach directly: the till connects
*out*, so no port forwarding, no dynamic DNS, no firewall exception.

Use it when the backend is in the cloud (orders arrive from a web POS, a kitchen display,
an online shop). Do **not** use it when the backend is on the same LAN as the till — the
local WebSocket on `localhost:9567` is faster, private, and simpler.

```
   backend / POS server
        |  HTTPS + X-API-Key
        v
   Cloudflare Worker  (auth: API key)            relay/src/index.ts
        |  Durable Object stub
        v
   Durable Object per agent  (job queue + acks)  relay/src/relay_do.ts
        ^  one outbound WebSocket, hibernatable, authenticated with the
        |  agent's own secret (stored as a SHA-256, never in plain text)
        |
   PrintBridge agent on the till
        |  the same local queue the SDK uses
        v
   printer -> paper
```

## The idea in one paragraph

The agent keeps one outbound WebSocket to its own Durable Object. The backend posts a job
to the relay, the relay hands it to that socket. If the till is offline the job waits in
the Durable Object for up to 24 hours and is sent when the agent reconnects. The agent
acknowledges twice: **accepted** (it is in the local queue, here is my local job id) and
**completed** (it printed, or here is why it did not). That second acknowledgement is the
point of the design — without it a backend can only tell that a job was *sent*, never that
a receipt came out of a printer.

## Protocol

Agent → relay, over the socket:

| Frame | Meaning |
|---|---|
| `{"type":"hello","agent_id":"...","version":"0.1.0"}` | Sent once on connect, so `status` can report what is on the other end |
| `{"type":"ping"}` | Every 30 s; the relay answers `pong` |
| `{"type":"job_result","id":"relay_...","stage":"accepted","status":"accepted","local_job_id":"..."}` | Job is in the local queue |
| `{"type":"job_result","id":"relay_...","stage":"completed","status":"success"}` | Job printed |
| `{"type":"job_result","id":"relay_...","stage":"completed","status":"failed","error":"..."}` | Job failed for good; `error` is the printer's own message |

Relay → agent:

| Frame | Meaning |
|---|---|
| `{"version":1,"id":"relay_...","type":"print","payload":{"printer":"...","data":"<base64>","job_name":"..."}}` | The same payload a local `print` request carries |
| `{"type":"pong"}` | Answer to a ping |

A job is only forgotten by the relay once the agent has acknowledged it. If the agent
crashes between receiving a job and acknowledging it, the relay re-sends the job when the
agent comes back — at-least-once, so the same receipt may be queued twice. The agent's
idempotency handling and the till operator's eyes deal with that; a lost order is worse
than a duplicate one.

## Deploying

Prerequisites: a Cloudflare account and Node 22.6 or newer (developed against Node 24; the
tests use Node's built-in TypeScript type stripping rather than a test framework).

```bash
cd relay
npm install

# 1. Create the KV namespace that holds the per-agent secret hashes.
npx wrangler kv namespace create AGENT_SECRETS
#    paste the printed id into wrangler.toml (AGENT_SECRETS -> id)

# 2. The shared key backends present on the REST API.
npx wrangler secret put RELAY_API_KEY

# 3. Ship it. The first deploy applies the SQLite Durable Object migration.
npx wrangler deploy
```

Check the bundle before deploying anything real — this is also the fastest way to see
whether your `wrangler.toml` is still valid:

```bash
npm run check          # tsc --noEmit + node --test
npm run deploy:dry-run # bundles, prints the bindings, uploads nothing
```

The dry run prints both bindings when the configuration is right:

```
Total Upload: 13.07 KiB / gzip: 3.97 KiB
Your Worker has access to the following bindings:
env.AGENT_RELAY (AgentRelayDO)                             Durable Object
env.AGENT_SECRETS (REPLACE_WITH_YOUR_KV_NAMESPACE_ID)      KV Namespace
```

### Registering an agent

Each agent gets its own secret. Only the hash is stored, so a KV dump does not hand
anyone a working credential.

```powershell
# PowerShell: make a secret and print its hash
$secret = -join ((1..32) | ForEach-Object { '{0:x}' -f (Get-Random -Max 16) })
$secret
(Get-FileHash -InputStream ([IO.MemoryStream]::new([Text.Encoding]::UTF8.GetBytes($secret))) -Algorithm SHA256).Hash.ToLower()
```

```bash
# bash: same thing
secret=$(openssl rand -hex 32); echo "$secret"
printf '%s' "$secret" | sha256sum | cut -d' ' -f1
```

Store the **hash** under the agent id, and keep the secret for the till's config:

```bash
npx wrangler kv key put --binding AGENT_SECRETS "till-14" "<the 64 character hash>"
```

The agent id must match the one in the agent's config: the relay derives the Durable
Object from the URL, so `till-14` here and `till-15` there are two different agents, and
the secret of one does not open the queue of the other.

### Configuring the agent

In `config.json` on the till:

```json
{
  "relay": {
    "url": "wss://printbridge-relay.<your-subdomain>.workers.dev",
    "agent_id": "till-14",
    "api_key": "<the secret from above>"
  }
}
```

The same can be passed on the command line (`-relay-url`, `-agent-id`, `-api-key`), which
is handy for a first test. The relay is opt-in: leave `relay.url` empty and the agent never
dials out.

Restart the agent. A working connection logs:

```
[Relay] Connecting outbound Cloud Relay socket to wss://.../ws/agent/till-14/ws
[Relay] Outbound Cloud Relay connection active for Agent ID: till-14
[Relay] Received remote cloud job relay_0f1c... -> enqueued local job 3d9a... on "Front Counter"
[Relay] Local job 3d9a... for cloud job relay_0f1c... finished: success
```

A misconfiguration is explicit rather than silent — the relay's own explanation is logged
and the reconnect backs off instead of hammering:

```
[Relay] Cloud Relay socket closed (relay refused the connection: HTTP 401 (unauthorized) —
        check relay.agent_id and relay.api_key). Reconnecting in 6s...
```

## Driving it from a backend

```bash
curl -X POST "https://printbridge-relay.<sub>.workers.dev/v1/agents/till-14/print" \
  -H "X-API-Key: $RELAY_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"printer":"Front Counter","data":"G0BIZWxsbw==\n","job_name":"Order 4471"}'
```

```json
{"success":true,"status":"delivered","job_id":"relay_9f2c..."}
```

`status` is `delivered` when an agent was connected and the job went down the socket, or
`queued_offline` when it is parked until the agent returns. Either way the answer is `202`
and the job id is what you poll:

```bash
curl -H "X-API-Key: $RELAY_API_KEY" \
  "https://printbridge-relay.<sub>.workers.dev/v1/agents/till-14/jobs/relay_9f2c..."
```

```json
{
  "job": {
    "id": "relay_9f2c...",
    "printer": "Front Counter",
    "jobName": "Order 4471",
    "status": "success",
    "createdAt": 1780000000000,
    "deliveredAt": 1780000000100,
    "ackedAt": 1780000000200,
    "completedAt": 1780000011000,
    "localJobId": "3d9a..."
  }
}
```

`GET /v1/agents/till-14/status` is the health view of one till:

```json
{
  "agent_id": "till-14",
  "agent_version": "0.1.0",
  "agent_connected_at": 1780000000000,
  "agent_online": true,
  "pending_jobs": 0,
  "stored_jobs": 4,
  "counts": { "success": 3, "failed": 1 },
  "last_results": [ ... the 20 most recent jobs, newest first ... ]
}
```

A backend that only wants fire-and-forget can ignore the job id; one that must not print a
receipt twice should poll, or use its own order id as the print content's identity — the
agent's local `idempotency_key` is not carried across the relay, so deduplication at the
relay is by job id, and a retry there means a second local job.

## API reference

All `/v1/...` routes require `X-API-Key: <RELAY_API_KEY>` (or the same value as a bearer
token). Requests are not CORS-enabled: this API is for a backend, not for a browser page.

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/v1/agents/:agentId/print` | Queue a job for that agent |
| `GET` | `/v1/agents/:agentId/status` | Online state, pending count, recent results |
| `GET` | `/v1/agents/:agentId/jobs/:jobId` | One job, including the local job id and any error |
| `GET` | `/ws/agent/:agentId/ws` | The agent's socket (bearer secret, not the API key) |
| `GET` | `/` | Service description and which auth modes are configured |

`POST /print` body:

| Field | Required | Notes |
|---|---|---|
| `printer` | yes | Printer name **as the agent knows it**, or a role name. Must exist on the till. |
| `data` | yes | Base64. Identical to the local protocol's `data`. Line breaks are tolerated, spaces are not. Max 4 MB of base64. |
| `job_name` | no | Shows in the spooler and the agent log. Defaults to `Remote Cloud Print Job`. |

Errors:

| Status | Body | Cause |
|---|---|---|
| `401` | `{"error":"unauthorized"}` | Wrong or missing agent secret, or an unknown agent id |
| `401` | `{"error":"invalid API key"}` | Wrong backend key |
| `503` | `{"error":"agent authentication is not configured: bind an AGENT_SECRETS KV namespace"}` | The relay was deployed without its KV binding |
| `503` | `{"error":"backend authentication is not configured: set the RELAY_API_KEY secret"}` | No `RELAY_API_KEY` set |
| `400` | `{"error":"'data' must be base64"}` | Malformed body |
| `404` | `{"error":"job not found"}` | Job expired (24 h) or the id belongs to another agent |

## Security model

- **Fail closed.** With no KV binding or no `RELAY_API_KEY` the relay refuses everything
  and says why. A relay that accepts any caller as any agent is an open print server on
  the internet, so "not configured" must never mean "open".
- **Per-agent secrets, stored hashed.** `AGENT_SECRETS` maps agent id → SHA-256 hex. The
  plaintext secret exists only on the till and in the operator's password manager.
- **Constant-time comparison** for both secrets, so the comparison cannot be timed.
- **Unknown agent ids are indistinguishable from wrong secrets** (both `401 unauthorized`),
  so the relay does not confirm which tills exist.
- **Objects are derived from the agent id**, and the Worker authenticates *before*
  forwarding to the object. The Durable Object trusts its caller and keeps no credentials.
- **Not browser-callable.** No CORS headers, so a page in a till's browser cannot use the
  relay API with a key that leaked into front-end code.
- **Bounded storage.** Jobs expire after 24 hours and at most 500 are kept per agent, so a
  misbehaving backend cannot fill a Durable Object's storage.

What this model does **not** give you: a stolen agent secret is enough to impersonate that
till (rotate it by putting a new hash in KV and updating the till), and the backend key is
shared by every backend you have (one key, not one per tenant). Per-tenant keys and job
level scoping are the natural next step if the relay stops being a single-tenant tool.

## Operations

| Concern | Behaviour |
|---|---|
| Cost | One Durable Object per agent, hibernating between jobs: an idle till costs storage and little else. The Worker is request-priced. |
| Agent offline | Jobs queue in the object and flush on reconnect, oldest first. |
| Agent crashes mid-job | The job was never acknowledged, so it is re-sent on reconnect. At-least-once. |
| Relay redeployed | The socket drops, the agent reconnects with backoff (3 s, doubling to 60 s), queued jobs flush. |
| Logs | `[observability] enabled = true` in `wrangler.toml`: `npx wrangler tail`. |
| Rotating the backend key | `npx wrangler secret put RELAY_API_KEY`, then update the backend. |
| Rotating an agent secret | Generate a new secret, `kv key put` the new hash, update the till's config, restart it. |
| Clock | All timestamps are the relay's; the agent's local job times stay local. |

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| Agent logs `HTTP 503 (agent authentication is not configured...)` | The KV namespace is not bound. Create it, paste the id into `wrangler.toml`, redeploy. |
| Agent logs `HTTP 401 (unauthorized)` | The stored hash and the configured secret disagree. Re-hash the exact secret (`printf '%s'` — a trailing newline changes the hash), or check the agent id matches the KV key. |
| Backend gets `503 not configured` on `/print` | `RELAY_API_KEY` was never set: `npx wrangler secret put RELAY_API_KEY`. |
| Backend gets `401 invalid API key` | The header is missing, or the key differs from the deployed secret. |
| `print` returns `queued_offline` and nothing ever prints | No agent socket. Check `/status`: `agent_online` should be true, and `agent_version` tells you which agent is connected. |
| Job sits in `delivered` and never gets `ackedAt` | The agent received the frame but did not queue it (bad base64, unknown printer) and the rejection ack was lost, or the till's process is stale. Check the till's agent log for `[Relay] Received remote cloud job` / `Rejected remote cloud job`. |
| Job is `failed` with `The printer name is invalid` | The `printer` field must match a printer installed on that till (or a role assigned to one). `list_printers` through the local SDK, or the dashboard, shows the exact strings. |
| `wrangler deploy` rejects the migration | You are reusing tag `v1` with different migrations. Deploy a new tag, or recreate the namespace — the relay stores nothing worth migrating. |
| `Cannot find module '@cloudflare/workers-types'` | `npm install` was not run in `relay/`. |
| The same receipt prints twice after a relay restart | Expected at-least-once behaviour: the agent crashed or dropped before acknowledging, so the job was re-sent. Acknowledge-based deduplication happens at the relay; per-order deduplication is the backend's job. |



