// PrintBridge protocol smoke test, driven through the built SDK.
//
// Run by deep_qa.ps1, which starts a throwaway agent first. It is deliberately safe: it
// never touches a real printer unless one is named on the command line, and it works with
// max_attempts = 1 so the dead-letter path is quick.
import { createRequire } from 'node:module';

const require = createRequire(import.meta.url);
const { PrintBridgeClient } = require('../../sdk/dist/index.js');

const port = Number(process.env.PB_QA_PORT || 9799);
const realPrinter = (process.env.PB_QA_PRINTER || '').trim();
const missingPrinter = 'PrintBridge QA Missing Printer';
const roleName = 'qa-station';

let failures = 0;

function check(name, ok, detail = '') {
  if (ok) {
    console.log(`[ok]   ${name}${detail ? ' :: ' + detail : ''}`);
  } else {
    failures++;
    console.log(`[FAIL] ${name}${detail ? ' :: ' + detail : ''}`);
  }
}

async function waitForTerminal(pb, jobId, timeoutMs = 20000) {
  const deadline = Date.now() + timeoutMs;
  let last = null;
  while (Date.now() < deadline) {
    last = await pb.jobs.status(jobId);
    if (last.status === 'success' || last.status === 'failed') {
      return last;
    }
    await new Promise((resolve) => setTimeout(resolve, 250));
  }
  return last;
}

const pb = new PrintBridgeClient({
  host: 'localhost',
  port,
  secure: false,
  autoConnect: false,
  requestTimeout: 10000
});

try {
  await pb.connect();
  check('connected to the agent', pb.connected, pb.url);

  // Pairing: the code must come from the dashboard, never from the socket.
  const requested = await pb.pairing.request('PrintBridge QA');
  check('request_pairing does not return the code', requested.code_requested === true && !('code' in requested), JSON.stringify(requested));

  const summary = await (await fetch(`http://localhost:${port}/api/dashboard/summary`)).json();
  check('dashboard exposes a 6-digit pairing code', typeof summary.pairing_code === 'string' && summary.pairing_code.length === 6);

  await pb.pairing.confirm(summary.pairing_code);
  check('paired', pb.pairing.isPaired());

  const printers = await pb.printers.find();
  check('list_printers returned an array', Array.isArray(printers), `${printers.length} printer(s)`);

  // Roles resolve against the live printer list.
  const target = realPrinter || (printers[0] && printers[0].name) || missingPrinter;
  await pb.roles.assign(roleName, target, { label: 'QA station', capabilities: { max_width: 32 } });
  let roles = await pb.roles.list();
  let mine = roles.find((r) => r.role === roleName);
  check('role assigned and resolved', Boolean(mine && mine.resolved), JSON.stringify(mine));
  check('capability profile round-tripped', Boolean(mine && mine.capabilities.max_width === 32));

  const diagnosis = await pb.diagnosePrinter({ role: roleName });
  check('diagnosis produced plain-language checks', diagnosis.printer === target && Array.isArray(diagnosis.checks) && diagnosis.checks.length > 0, (diagnosis.checks || []).join(' | '));

  const status = await pb.queue.status();
  check('queue_status reports counts', typeof status.counts.queued === 'number', JSON.stringify(status.counts));

  // A role that points at a printer that is not installed must fail clearly.
  await pb.roles.assign(roleName, missingPrinter, { label: 'QA station' });
  let unresolvedError = '';
  try {
    await pb.printToRole(roleName, new Uint8Array([0x1B, 0x40, 0x51, 0x41]), { jobName: 'QA unresolved' });
  } catch (err) {
    unresolvedError = String(err && err.message);
  }
  check('an unresolved role fails with a clear error', unresolvedError.includes('not installed'), unresolvedError);

  // The queue must accept a job for a printer that is not there, then dead-letter it.
  const payload = new Uint8Array([0x1B, 0x40, 0x51, 0x41, 0x0A, 0x1B, 0x64, 0x03]);
  const { jobId, duplicate } = await pb.print(missingPrinter, payload, { jobName: 'QA job', idempotencyKey: 'qa-1' });
  check('print accepted', typeof jobId === 'string' && duplicate === false, jobId);

  const again = await pb.print(missingPrinter, payload, { jobName: 'QA job', idempotencyKey: 'qa-1' });
  check('idempotency key deduplicated the job', again.jobId === jobId && again.duplicate === true);

  const final = await waitForTerminal(pb, jobId);
  check('job reached a terminal state', Boolean(final && (final.status === 'success' || final.status === 'failed')), final && `${final.status} after ${final.attempts} attempt(s)`);
  check('job dead-lettered with its error recorded', Boolean(final && final.status === 'failed' && final.error_message), final && final.error_message);

  await pb.queue.retryJob(jobId);
  const retried = await waitForTerminal(pb, jobId);
  check('retry_job re-ran the job', Boolean(retried && retried.status === 'failed'), retried && `${retried.status} after ${retried.attempts} attempt(s)`);

  const cleared = await pb.queue.clearFailed();
  check('clear_failed_jobs removed the dead letter', Number(cleared.removed) >= 1, String(cleared.removed));

  // A real printer, when one was named, must actually print.
  if (realPrinter) {
    const printed = await pb.print(realPrinter, payload, { jobName: 'QA real print', idempotencyKey: 'qa-real' });
    const result = await waitForTerminal(pb, printed.jobId);
    check('real print succeeded', Boolean(result && result.status === 'success'), result && (result.error_message || result.status));
  } else {
    console.log('[skip] real print: pass -Printer "<name>" to exercise hardware');
  }

  await pb.roles.remove(roleName);
  roles = await pb.roles.list();
  check('role removed', !roles.some((r) => r.role === roleName));
} catch (err) {
  failures++;
  console.log(`[FAIL] unexpected error: ${err && err.message}`);
} finally {
  pb.disconnect();
}

console.log(failures === 0 ? '\nQA PASSED' : `\nQA FAILED (${failures} check(s))`);
process.exit(failures === 0 ? 0 : 1);
