/**
 * PrintBridge Cloud Relay — the public front door.
 *
 * Two callers exist, and they are authenticated differently:
 *
 *   Agents   connect to `GET /ws/agent/:agentId` and must present their own secret as
 *            `Authorization: Bearer <secret>`. Only the SHA-256 of the secret is stored,
 *            in the AGENT_SECRETS KV namespace, under the agent's id.
 *   Backends call `POST /v1/agents/:agentId/print` and friends with the shared relay API
 *            key in the `X-API-Key` header (or as a bearer token).
 *
 * Both checks fail closed: if the relay has not been configured, it refuses the request
 * with an explanation instead of accepting an anonymous caller. That matters, because a
 * relay that accepts anyone as any agent is an open print server on the internet.
 */
import { AgentRelayDO } from './relay_do.ts';
import {
  bearerToken,
  constantTimeEqual,
  jsonResponse,
  sha256Hex,
  type Env
} from './lib.ts';

export { AgentRelayDO };

interface AuthOutcome {
  ok: boolean;
  status: number;
  error?: string;
}

export default {
  async fetch(request: Request, env: Env): Promise<Response> {
    const url = new URL(request.url);
    const parts = url.pathname.split('/').filter(Boolean);

    // Agent socket: /ws/agent/:agentId
    if (parts[0] === 'ws' && parts[1] === 'agent' && parts[2]) {
      const auth = await authenticateAgent(request, env, decodeURIComponent(parts[2]));
      if (!auth.ok) {
        return jsonResponse({ error: auth.error }, auth.status);
      }
      return env.AGENT_RELAY.get(env.AGENT_RELAY.idFromName(parts[2])).fetch(request);
    }

    // Backend REST: /v1/agents/:agentId/...
    if (parts[0] === 'v1' && parts[1] === 'agents' && parts[2]) {
      const auth = authenticateBackend(request, env);
      if (!auth.ok) {
        return jsonResponse({ error: auth.error }, auth.status);
      }
      return env.AGENT_RELAY.get(env.AGENT_RELAY.idFromName(parts[2])).fetch(request);
    }

    return jsonResponse({
      service: 'printbridge-relay',
      protocol_version: 1,
      agent_auth: env.AGENT_SECRETS ? 'per-agent secret (KV)' : 'not configured',
      backend_auth: env.RELAY_API_KEY ? 'api key' : 'not configured',
      endpoints: {
        agent_socket: 'GET /ws/agent/:agentId',
        print: 'POST /v1/agents/:agentId/print',
        status: 'GET /v1/agents/:agentId/status',
        job: 'GET /v1/agents/:agentId/jobs/:jobId'
      }
    });
  }
};

/** Verifies an agent's secret against the SHA-256 stored for its id. */
async function authenticateAgent(request: Request, env: Env, agentId: string): Promise<AuthOutcome> {
  if (!env.AGENT_SECRETS) {
    return {
      ok: false,
      status: 503,
      error: 'agent authentication is not configured: bind an AGENT_SECRETS KV namespace'
    };
  }

  const provided = bearerToken(request.headers.get('Authorization'));
  if (provided === '') {
    return { ok: false, status: 401, error: 'missing Authorization: Bearer <agent secret>' };
  }

  const expected = await env.AGENT_SECRETS.get(agentId);
  if (!expected) {
    // No secret registered for this id. The message is intentionally the same as for a
    // wrong secret, so the relay does not confirm which agent ids exist.
    return { ok: false, status: 401, error: 'unauthorized' };
  }

  const providedHash = await sha256Hex(provided);
  if (!constantTimeEqual(expected.trim().toLowerCase(), providedHash)) {
    return { ok: false, status: 401, error: 'unauthorized' };
  }

  return { ok: true, status: 200 };
}

/** Verifies the backend's relay API key. */
function authenticateBackend(request: Request, env: Env): AuthOutcome {
  if (!env.RELAY_API_KEY) {
    return {
      ok: false,
      status: 503,
      error: 'backend authentication is not configured: set the RELAY_API_KEY secret'
    };
  }

  const provided = request.headers.get('X-API-Key') ?? bearerToken(request.headers.get('Authorization'));
  if (provided === '') {
    return { ok: false, status: 401, error: 'missing X-API-Key header' };
  }

  if (!constantTimeEqual(env.RELAY_API_KEY, provided)) {
    return { ok: false, status: 401, error: 'invalid API key' };
  }

  return { ok: true, status: 200 };
}

