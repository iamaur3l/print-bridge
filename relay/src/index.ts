import { AgentRelayDO, Env } from './relay_do';

export { AgentRelayDO };

export default {
  async fetch(request: Request, env: Env): Promise<Response> {
    const url = new URL(request.url);
    const pathParts = url.pathname.split('/').filter(Boolean);

    // Route: /ws/agent/:agentId -> Durable Object
    if (pathParts[0] === 'ws' && pathParts[1] === 'agent' && pathParts[2]) {
      const agentId = pathParts[2];
      const id = env.AGENT_RELAY.idFromName(agentId);
      const stub = env.AGENT_RELAY.get(id);
      return stub.fetch(request);
    }

    // Route: /v1/agents/:agentId/print or /v1/agents/:agentId/status
    if (pathParts[0] === 'v1' && pathParts[1] === 'agents' && pathParts[2]) {
      const agentId = pathParts[2];
      const id = env.AGENT_RELAY.idFromName(agentId);
      const stub = env.AGENT_RELAY.get(id);
      return stub.fetch(request);
    }

    return new Response('PrintBridge Cloud Relay API', { status: 200 });
  }
};
