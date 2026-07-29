export interface Env {
  AGENT_RELAY: DurableObjectNamespace;
}

export class AgentRelayDO {
  private state: DurableObjectState;
  private agentWs: WebSocket | null = null;
  private pendingQueue: Array<{ id: string; printer: string; data: string; jobName: string; createdAt: number }> = [];

  constructor(state: DurableObjectState, env: Env) {
    this.state = state;
    this.state.blockConcurrencyWhile(async () => {
      const storedQueue = await this.state.storage.get<typeof this.pendingQueue>('pending_queue');
      if (storedQueue) {
        // Filter out expired jobs (> 24 hours)
        const cutoff = Date.now() - 24 * 60 * 60 * 1000;
        this.pendingQueue = storedQueue.filter(j => j.createdAt > cutoff);
      }
    });
  }

  async fetch(request: Request): Promise<Response> {
    const url = new URL(request.url);

    // Agent WebSocket connection
    if (url.pathname.endsWith('/ws')) {
      const upgradeHeader = request.headers.get('Upgrade');
      if (!upgradeHeader || upgradeHeader.toLowerCase() !== 'websocket') {
        return new Response('Expected WebSocket', { status: 426 });
      }

      const pair = new WebSocketPair();
      const [client, server] = Object.values(pair);

      server.accept();
      this.agentWs = server;

      server.addEventListener('message', async (evt) => {
        try {
          const msg = JSON.parse(evt.data as string);
          if (msg.type === 'ping') {
            server.send(JSON.stringify({ type: 'pong' }));
          }
        } catch (e) {}
      });

      server.addEventListener('close', () => {
        if (this.agentWs === server) {
          this.agentWs = null;
        }
      });

      // Deliver pending offline queued jobs upon reconnect
      this.flushPendingQueue();

      return new Response(null, { status: 101, webSocket: client });
    }

    // Backend REST API print dispatch: POST /print
    if (request.method === 'POST' && url.pathname.endsWith('/print')) {
      const body = await request.json() as { printer: string; data: string; job_name?: string };
      if (!body.printer || !body.data) {
        return new Response(JSON.stringify({ success: false, error: "Missing required 'printer' or 'data' fields" }), {
          status: 400,
          headers: { 'Content-Type': 'application/json' }
        });
      }

      const jobId = `relay_${crypto.randomUUID()}`;
      const printJob = {
        id: jobId,
        printer: body.printer,
        data: body.data,
        jobName: body.job_name || 'Remote Cloud Print Job',
        createdAt: Date.now()
      };

      if (this.agentWs) {
        this.agentWs.send(JSON.stringify({
          version: 1,
          id: jobId,
          type: 'print',
          payload: {
            printer: printJob.printer,
            data: printJob.data,
            job_name: printJob.jobName
          }
        }));
        return new Response(JSON.stringify({ success: true, status: 'delivered', job_id: jobId }), {
          headers: { 'Content-Type': 'application/json' }
        });
      } else {
        // Agent offline: queue job in Durable Object state (24h TTL)
        this.pendingQueue.push(printJob);
        await this.state.storage.put('pending_queue', this.pendingQueue);
        return new Response(JSON.stringify({ success: true, status: 'queued_offline', job_id: jobId }), {
          headers: { 'Content-Type': 'application/json' }
        });
      }
    }

    // Agent status REST API: GET /status
    if (request.method === 'GET' && url.pathname.endsWith('/status')) {
      return new Response(JSON.stringify({
        agent_online: this.agentWs !== null,
        pending_jobs: this.pendingQueue.length
      }), {
        headers: { 'Content-Type': 'application/json' }
      });
    }

    return new Response('Not Found', { status: 404 });
  }

  private async flushPendingQueue() {
    if (!this.agentWs || this.pendingQueue.length === 0) return;

    for (const job of this.pendingQueue) {
      this.agentWs.send(JSON.stringify({
        version: 1,
        id: job.id,
        type: 'print',
        payload: {
          printer: job.printer,
          data: job.data,
          job_name: job.jobName
        }
      }));
    }

    this.pendingQueue = [];
    await this.state.storage.delete('pending_queue');
  }
}
