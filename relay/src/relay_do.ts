/**
 * One Durable Object per agent.
 *
 * It holds the agent's outbound WebSocket (using the hibernation API, so the object can
 * be evicted between jobs without dropping the connection), queues jobs while the agent
 * is away, and records the agent's acknowledgements so a backend can find out what
 * actually happened to a remote print.
 */
import {
  isPending,
  jsonResponse,
  pruneJobs,
  recentResults,
  validatePrintRequest,
  type RelayJob,
  type RelayJobStatus
} from './lib.ts';
const JOB_PREFIX = 'job:';
const AGENT_KEY = 'agent';
/** Storage is not free, so only the newest N jobs are kept. */
const MAX_STORED_JOBS = 500;

interface AgentInfo {
  agentId: string;
  version: string;
  connectedAt: number;
}

export class AgentRelayDO implements DurableObject {
  // No env is needed here: callers are authenticated at the Worker edge before the
  // request reaches the object, and the object holds all of its state in its own
  // storage rather than in a binding.
  constructor(private readonly state: DurableObjectState) {}

  async fetch(request: Request): Promise<Response> {
    const url = new URL(request.url);

    if (url.pathname.endsWith('/ws')) {
      return this.handleAgentSocket(request);
    }

    if (request.method === 'POST' && url.pathname.endsWith('/print')) {
      return this.handlePrint(request);
    }

    if (request.method === 'GET' && url.pathname.includes('/jobs/')) {
      const jobId = url.pathname.split('/').pop() ?? '';
      const job = await this.state.storage.get<RelayJob>(JOB_PREFIX + jobId);
      if (!job) return jsonResponse({ error: 'job not found' }, 404);
      return jsonResponse({ job });
    }

    if (request.method === 'GET' && url.pathname.endsWith('/status')) {
      return this.handleStatus();
    }

    return jsonResponse({ error: 'not found' }, 404);
  }

  // --- agent socket -----------------------------------------------------------

  private async handleAgentSocket(request: Request): Promise<Response> {
    const upgrade = request.headers.get('Upgrade')?.toLowerCase();
    if (upgrade !== 'websocket') {
      return jsonResponse({ error: 'expected a WebSocket upgrade' }, 426);
    }

    const pair = new WebSocketPair();
    const [client, server] = Object.values(pair);

    // Hibernation: the runtime owns the socket, so this object can be evicted and
    // still receive webSocketMessage events afterwards.
    const previous = this.state.getWebSockets();
    this.state.acceptWebSocket(server);

    // One live socket per agent. A reconnecting agent replaces its old connection
    // instead of leaving a half-open one that jobs could be delivered into.
    for (const socket of previous) {
      try {
        socket.close(1000, 'replaced by a new agent connection');
      } catch {
        // already gone
      }
    }

    const agentId = new URL(request.url).pathname.split('/').filter(Boolean)[2] ?? '';
    await this.state.storage.put(AGENT_KEY, {
      agentId,
      version: '',
      connectedAt: Date.now()
    } satisfies AgentInfo);

    await this.flushPendingJobs();

    return new Response(null, { status: 101, webSocket: client });
  }

  async webSocketMessage(ws: WebSocket, message: string | ArrayBuffer): Promise<void> {
    const text = typeof message === 'string' ? message : new TextDecoder().decode(message);

    let parsed: Record<string, unknown>;
    try {
      parsed = JSON.parse(text) as Record<string, unknown>;
    } catch {
      return; // a malformed frame must not break the socket
    }

    switch (parsed.type) {
      case 'ping':
        try {
          ws.send(JSON.stringify({ type: 'pong', time: Date.now() }));
        } catch {
          // The socket closed between the frame arriving and the answer: harmless.
        }
        return;
      case 'hello':
        await this.rememberAgent(parsed);
        return;
      case 'job_result':
        await this.recordResult(parsed);
        return;
      default:
        return;
    }
  }

  async webSocketClose(): Promise<void> {
    // Nothing to do: the runtime removes the socket from getWebSockets().
  }

  async webSocketError(): Promise<void> {
    // Same as close: the connection is already gone from this object's point of view.
  }

  // --- remote print -----------------------------------------------------------

  private async handlePrint(request: Request): Promise<Response> {
    let body: unknown;
    try {
      body = await request.json();
    } catch {
      return jsonResponse({ error: 'body must be JSON' }, 400);
    }

    const validation = validatePrintRequest(body);
    if (!validation.ok) {
      return jsonResponse({ success: false, error: validation.error }, 400);
    }

    const job: RelayJob = {
      id: `relay_${crypto.randomUUID()}`,
      printer: validation.printer,
      data: validation.data,
      jobName: validation.jobName,
      createdAt: Date.now(),
      status: 'queued'
    };

    // Store before sending: a job the agent has printed must not outrun its record.
    await this.housekeep();
    await this.put(job);

    const delivered = await this.deliver(job);
    return jsonResponse(
      { success: true, status: delivered ? 'delivered' : 'queued_offline', job_id: job.id },
      202
    );
  }

  /** Sends one job down the agent's socket. False when no agent is connected. */
  private async deliver(job: RelayJob): Promise<boolean> {
    // The newest socket: an agent that reconnected replaced its old one above.
    const sockets = this.state.getWebSockets();
    const socket = sockets[sockets.length - 1];
    if (!socket) return false;

    try {
      socket.send(
        JSON.stringify({
          version: 1,
          id: job.id,
          type: 'print',
          payload: { printer: job.printer, data: job.data, job_name: job.jobName }
        })
      );
    } catch {
      return false; // the socket died between the check and the send
    }

    job.deliveredAt = Date.now();
    job.status = 'delivered';
    await this.put(job);
    return true;
  }

  /**
   * Re-sends everything the agent has not acknowledged. A job is only forgotten once
   * the agent says it received it, so a crash mid-print repeats the job rather than
   * losing it. The agent's own idempotency handling deals with the repeat.
   */
  private async flushPendingJobs(): Promise<void> {
    for (const job of (await this.allJobs()).filter(isPending)) {
      if (!(await this.deliver(job))) return;
    }
  }

  /** Applies an acknowledgement from the agent. */
  private async recordResult(message: Record<string, unknown>): Promise<void> {
    const jobId = typeof message.id === 'string' ? message.id : '';
    if (jobId === '') return;

    const job = await this.state.storage.get<RelayJob>(JOB_PREFIX + jobId);
    if (!job) return; // expired, or never ours

    const stage = typeof message.stage === 'string' ? message.stage : '';
    const status = typeof message.status === 'string' ? (message.status as RelayJobStatus) : undefined;
    const localJobId = typeof message.local_job_id === 'string' ? message.local_job_id : undefined;
    const error = typeof message.error === 'string' ? message.error : undefined;
    const now = Date.now();

    if (stage === 'accepted') {
      job.ackedAt = now;
      job.status = status ?? 'accepted';
      if (localJobId !== undefined) job.localJobId = localJobId;
    }

    if (stage === 'completed') {
      job.completedAt = now;
      job.status = status ?? 'failed';
      if (error !== undefined) job.error = error;
    }

    await this.put(job);
  }

  private async rememberAgent(message: Record<string, unknown>): Promise<void> {
    const info = await this.state.storage.get<AgentInfo>(AGENT_KEY);
    await this.state.storage.put(AGENT_KEY, {
      agentId: (message.agent_id as string) ?? info?.agentId ?? '',
      version: (message.version as string) ?? '',
      connectedAt: info?.connectedAt ?? Date.now()
    } satisfies AgentInfo);
  }

  // --- status -----------------------------------------------------------------

  private async handleStatus(): Promise<Response> {
    const jobs = await this.allJobs();
    const agent = await this.state.storage.get<AgentInfo>(AGENT_KEY);

    const counts = jobs.reduce<Record<string, number>>((accumulator, job) => {
      accumulator[job.status] = (accumulator[job.status] ?? 0) + 1;
      return accumulator;
    }, {});

    return jsonResponse({
      agent_id: agent?.agentId ?? '',
      agent_version: agent?.version ?? '',
      agent_connected_at: agent?.connectedAt ?? null,
      agent_online: this.state.getWebSockets().length > 0,
      pending_jobs: jobs.filter(isPending).length,
      stored_jobs: jobs.length,
      counts,
      last_results: recentResults(jobs)
    });
  }

  // --- storage ----------------------------------------------------------------

  private async put(job: RelayJob): Promise<void> {
    await this.state.storage.put(JOB_PREFIX + job.id, job);
  }

  private async allJobs(): Promise<RelayJob[]> {
    const stored = await this.state.storage.list<RelayJob>({ prefix: JOB_PREFIX });
    return pruneJobs([...stored.values()], Date.now());
  }

  /** Drops expired jobs and caps how many are kept. */
  private async housekeep(): Promise<void> {
    const stored = await this.state.storage.list<RelayJob>({ prefix: JOB_PREFIX });
    const all = [...stored.entries()];
    const survivors = pruneJobs(
      all.map(([, job]) => job),
      Date.now()
    );

    const keep = new Set(survivors.slice(-MAX_STORED_JOBS).map((job) => job.id));
    const doomed = all.filter(([key, job]) => !keep.has(job.id) && key.startsWith(JOB_PREFIX)).map(([key]) => key);

    if (doomed.length > 0) {
      await this.state.storage.delete(doomed);
    }
  }
}
