/**
 * Pure helpers for the PrintBridge Cloud Relay.
 *
 * Everything in here is deliberately free of Cloudflare APIs so it can be unit tested
 * with `node --test` and no extra dependencies.
 */

/** A job on its way to, or arriving at, an agent. */
export interface RelayJob {
  id: string;
  printer: string;
  /** Base64 payload, exactly as the agent's local protocol carries it. */
  data: string;
  jobName: string;
  createdAt: number;
  deliveredAt?: number;
  /** Set when the agent acknowledges receipt. */
  ackedAt?: number;
  /** Set when the agent acknowledges the final print outcome. */
  completedAt?: number;
  status: RelayJobStatus;
  /** The agent's own job id, once it has accepted the job. */
  localJobId?: string;
  error?: string;
}

export type RelayJobStatus =
  | 'queued'
  | 'delivered'
  | 'accepted'
  | 'success'
  | 'failed';

/** Jobs are dropped a day after they were created, whether or not they were delivered. */
export const JOB_TTL_MS = 24 * 60 * 60 * 1000;

/** Base64 characters allowed for a payload (~3 MB of raw bytes). */
export const MAX_PAYLOAD_CHARS = 4 * 1024 * 1024;

/** How many recent results `status` reports. */
export const MAX_RECENT_RESULTS = 20;

/** True when the job has not been acknowledged yet and should be (re)sent. */
export function isPending(job: RelayJob): boolean {
  return job.ackedAt === undefined;
}

/** True when every attempt to print is over. */
export function isFinished(job: RelayJob): boolean {
  return job.status === 'success' || job.status === 'failed';
}

/** Drops jobs older than the TTL. Returns the survivors, newest last. */
export function pruneJobs(jobs: RelayJob[], now: number, ttlMs: number = JOB_TTL_MS): RelayJob[] {
  return jobs
    .filter((job) => now - job.createdAt < ttlMs)
    .sort((a, b) => a.createdAt - b.createdAt);
}

/** The most recent results, newest first, for the status endpoint. */
export function recentResults(jobs: RelayJob[], limit: number = MAX_RECENT_RESULTS): RelayJob[] {
  return jobs
    .filter((job) => job.deliveredAt !== undefined)
    .sort((a, b) => b.createdAt - a.createdAt)
    .slice(0, limit);
}

export type ValidationResult =
  | { ok: true; printer: string; data: string; jobName: string }
  | { ok: false; error: string };

/**
 * Validates a `POST /print` body. Bounds matter here: this endpoint is reachable from
 * the internet, and Durable Object storage is not free.
 */
export function validatePrintRequest(body: unknown): ValidationResult {
  if (typeof body !== 'object' || body === null) {
    return { ok: false, error: 'body must be a JSON object' };
  }

  const candidate = body as Record<string, unknown>;
  const printer = typeof candidate.printer === 'string' ? candidate.printer.trim() : '';
  const data = typeof candidate.data === 'string' ? candidate.data : '';
  const rawName = typeof candidate.job_name === 'string' ? candidate.job_name.trim() : '';

  if (printer === '') {
    return { ok: false, error: "missing required 'printer' field" };
  }
  if (printer.length > 255) {
    return { ok: false, error: "'printer' must be 255 characters or fewer" };
  }
  if (data === '') {
    return { ok: false, error: "missing required 'data' field (base64)" };
  }
  if (data.length > MAX_PAYLOAD_CHARS) {
    return { ok: false, error: `'data' exceeds the ${MAX_PAYLOAD_CHARS} character limit` };
  }
  if (!isBase64(data)) {
    return { ok: false, error: "'data' must be base64" };
  }
  if (rawName.length > 255) {
    return { ok: false, error: "'job_name' must be 255 characters or fewer" };
  }

  return { ok: true, printer, data, jobName: rawName === '' ? 'Remote Cloud Print Job' : rawName };
}

const BASE64_PATTERN = /^[A-Za-z0-9+/]*={0,2}$/;
/** Only line wrapping is tolerated: MIME base64 arrives in 76 column lines. */
const BASE64_LINE_BREAKS = /[\r\n]/g;

export function isBase64(value: string): boolean {
  if (value === '') return false;

  // Spaces and tabs are not tolerated: a payload containing them is far more likely to
  // be malformed than to be deliberate, and silently stripping them would hide that.
  const normalised = value.replace(BASE64_LINE_BREAKS, '');
  return BASE64_PATTERN.test(normalised);
}

/** Extracts a bearer token, or an empty string when there is none. */
export function bearerToken(header: string | null): string {
  if (!header) return '';
  const match = /^Bearer\s+(.+)$/i.exec(header.trim());
  return match ? match[1].trim() : '';
}

/**
 * Compares two strings without leaking their contents through timing. Length is
 * compared first; that reveals only the length, which for a 32-byte secret is not
 * useful to an attacker.
 */
export function constantTimeEqual(a: string, b: string): boolean {
  if (a.length !== b.length) return false;

  let diff = 0;
  for (let i = 0; i < a.length; i++) {
    diff |= a.charCodeAt(i) ^ b.charCodeAt(i);
  }
  return diff === 0;
}

/** SHA-256 of a value, hex encoded. Used to store agent secrets as hashes. */
export async function sha256Hex(value: string): Promise<string> {
  const digest = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(value));
  return [...new Uint8Array(digest)].map((byte) => byte.toString(16).padStart(2, '0')).join('');
}

/** True when a stored value looks like a hex-encoded SHA-256 digest. */
export function looksHashed(value: string): boolean {
  return /^[0-9a-f]{64}$/i.test(value.trim());
}

export interface Env {
  AGENT_RELAY: DurableObjectNamespace;
  /** Optional KV namespace mapping agent id -> SHA-256 hex of the agent's secret. */
  AGENT_SECRETS?: KVNamespace;
  /** Shared secret the backend presents on the REST API. */
  RELAY_API_KEY?: string;
}

/** JSON response helper. Relay data must never be cached by an intermediary. */
export function jsonResponse(
  body: unknown,
  status = 200,
  headers: Record<string, string> = {}
): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json', 'Cache-Control': 'no-store', ...headers }
  });
}
