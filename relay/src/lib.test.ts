/**
 * Unit tests for the relay's pure logic.
 *
 * Run with `npm test` in `relay/`. They use node:test and Node's built-in TypeScript
 * type stripping, so the relay needs no test framework or extra dependency.
 */
import { test } from 'node:test';
import assert from 'node:assert/strict';

import {
  bearerToken,
  constantTimeEqual,
  isBase64,
  isFinished,
  isPending,
  looksHashed,
  MAX_PAYLOAD_CHARS,
  pruneJobs,
  recentResults,
  sha256Hex,
  validatePrintRequest,
  type RelayJob
} from './lib.ts';

test('validatePrintRequest accepts a well formed body', () => {
  const result = validatePrintRequest({ printer: ' XP-80C ', data: 'G0BIZWxsbw==', job_name: ' Order 1 ' });
  assert.equal(result.ok, true);
  if (!result.ok) return;

  assert.equal(result.printer, 'XP-80C', 'printer is trimmed');
  assert.equal(result.jobName, 'Order 1', 'job name is trimmed');
  assert.equal(result.data, 'G0BIZWxsbw==');
});

test('validatePrintRequest defaults the job name', () => {
  const result = validatePrintRequest({ printer: 'XP-80C', data: 'G0A=' });
  assert.equal(result.ok, true);
  if (result.ok) assert.equal(result.jobName, 'Remote Cloud Print Job');
});

test('validatePrintRequest rejects bad input', () => {
  const cases: Array<[unknown, string]> = [
    [null, 'body must be a JSON object'],
    ['a string', 'body must be a JSON object'],
    [{ data: 'G0A=' }, "'printer'"],
    [{ printer: 'XP-80C' }, "'data'"],
    [{ printer: 'XP-80C', data: 'not base64!' }, 'must be base64'],
    [{ printer: 'x'.repeat(256), data: 'G0A=' }, '255 characters'],
    [{ printer: 'XP-80C', data: 'G0A=', job_name: 'y'.repeat(256) }, "'job_name'"]
  ];

  for (const [body, expected] of cases) {
    const result = validatePrintRequest(body);
    assert.equal(result.ok, false, `expected ${JSON.stringify(body)} to be rejected`);
    if (!result.ok) {
      assert.ok(result.error.includes(expected), `expected "${result.error}" to mention "${expected}"`);
    }
  }
});

test('validatePrintRequest enforces the payload limit', () => {
  const body = { printer: 'XP-80C', data: 'A'.repeat(MAX_PAYLOAD_CHARS + 4) };
  const result = validatePrintRequest(body);
  assert.equal(result.ok, false);
  if (!result.ok) assert.ok(result.error.includes('exceeds'));
});

test('isBase64 tolerates whitespace and rejects junk', () => {
  assert.equal(isBase64('G0BIZWxsbw=='), true);
  assert.equal(isBase64('G0BIZWxs\nbw=='), true, 'newlines from a pretty printer');
  assert.equal(isBase64(''), false);
  assert.equal(isBase64('has spaces'), false);
  assert.equal(isBase64('emoji🖨'), false);
});

test('bearerToken parses the Authorization header', () => {
  assert.equal(bearerToken('Bearer abc123'), 'abc123');
  assert.equal(bearerToken('bearer abc123'), 'abc123', 'scheme is case insensitive');
  assert.equal(bearerToken('  Bearer   abc123  '), 'abc123');
  assert.equal(bearerToken('abc123'), '', 'a bare token is refused');
  assert.equal(bearerToken(''), '');
  assert.equal(bearerToken(null), '');
});

test('constantTimeEqual compares without early exit', () => {
  assert.equal(constantTimeEqual('secret', 'secret'), true);
  assert.equal(constantTimeEqual('secret', 'secreT'), false);
  assert.equal(constantTimeEqual('secret', 'secret '), false, 'different length');
  assert.equal(constantTimeEqual('', ''), true);
  assert.equal(constantTimeEqual('a', ''), false);
});

test('sha256Hex matches known vectors', async () => {
  assert.equal(
    await sha256Hex(''),
    'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855'
  );
  assert.equal(
    await sha256Hex('abc'),
    'ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad'
  );
});

test('looksHashed recognises stored digests', () => {
  assert.equal(looksHashed('ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad'), true);
  assert.equal(looksHashed('BA7816BF8F01CFEA414140DE5DAE2223B00361A396177A9CB410FF61F20015AD'), true);
  assert.equal(looksHashed('hunter2'), false);
  assert.equal(looksHashed(''), false);
});

function job(overrides: Partial<RelayJob> = {}): RelayJob {
  return {
    id: 'job-1',
    printer: 'XP-80C',
    data: 'G0A=',
    jobName: 'Job',
    createdAt: 1_000,
    status: 'queued',
    ...overrides
  };
}

test('isPending and isFinished describe the job lifecycle', () => {
  assert.equal(isPending(job()), true);
  assert.equal(isPending(job({ ackedAt: 2_000 })), false, 'an acknowledged job is not re-sent');

  assert.equal(isFinished(job()), false);
  assert.equal(isFinished(job({ status: 'success' })), true);
  assert.equal(isFinished(job({ status: 'failed' })), true);
  assert.equal(isFinished(job({ status: 'accepted' })), false);
});

test('pruneJobs drops expired jobs and keeps chronological order', () => {
  const now = 100_000;
  const jobs = [
    job({ id: 'newest', createdAt: now - 10 }),
    job({ id: 'expired', createdAt: now - 25 * 60 * 60 * 1000 }),
    job({ id: 'oldest', createdAt: now - 500 })
  ];

  const kept = pruneJobs(jobs, now);
  assert.deepEqual(
    kept.map((entry) => entry.id),
    ['oldest', 'newest']
  );
});

test('recentResults reports only delivered jobs, newest first', () => {
  const jobs = [
    job({ id: 'queued', createdAt: 1 }),
    job({ id: 'old-delivered', createdAt: 2, deliveredAt: 2 }),
    job({ id: 'new-delivered', createdAt: 3, deliveredAt: 3, status: 'success', localJobId: 'local-1' })
  ];

  const results = recentResults(jobs);
  assert.deepEqual(
    results.map((entry) => entry.id),
    ['new-delivered', 'old-delivered'],
    'a job that was never delivered is not a result yet'
  );
  assert.equal(results[0].localJobId, 'local-1');

  assert.equal(recentResults(jobs, 1).length, 1, 'respects the limit');
});
