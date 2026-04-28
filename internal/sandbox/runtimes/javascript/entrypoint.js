#!/usr/bin/env node
/**
 * Sandbox entrypoint for JavaScript/TypeScript execution.
 *
 * Protocol:
 *   stdin  → JSON { code, input, context, params }
 *   stdout → JSON output (result of user code)
 *   stderr → logs / errors
 *
 * User code runs as ESM (.mjs) via dynamic `await import()` in BOTH run and
 * debug modes. Globals available in user code:
 *   input, context, params, output, console
 * Module loading: `const x = await import('pkg')`. CommonJS `require` is
 * intentionally not provided — ESM scope makes it impractical to expose,
 * and unifying on `await import` keeps run/debug behavior identical.
 */
'use strict';

const fs = require('fs');
const { spawn } = require('child_process');

process.stdin.setEncoding('utf8');

let raw = '';
process.stdin.on('data', (chunk) => { raw += chunk; });

process.stdin.on('end', async () => {
  let payload;
  try {
    payload = JSON.parse(raw);
  } catch (err) {
    process.stderr.write(`entrypoint: invalid JSON payload: ${err.message}\n`);
    process.exit(1);
  }

  const { code, input, context, params } = payload;

  // Setup script: sets globals via globalThis, loaded via -r flag (CJS).
  // Used by both run and debug modes.
  // NOTE (debug): do NOT override console — real console.log must fire so CDP
  // Runtime.consoleAPICalled events reach the debug UI console panel.
  // NOTE (run): console.log is redirected to stderr inside the runner so
  // stdout stays clean for the final JSON result.
  const setupScript = `'use strict';
globalThis.input = ${JSON.stringify(input ?? null)};
globalThis.context = ${JSON.stringify(context ?? {})};
globalThis.params = ${JSON.stringify(params ?? {})};
globalThis._outputValue = undefined;
globalThis._outputSet = false;
globalThis.output = (val) => { globalThis._outputValue = val; globalThis._outputSet = true; };
`;

  // User script: pure user code, line 1 = user line 1 (preserves breakpoints).
  const userScript = code;

  if (process.env.SANDBOX_DEBUG) {
    // Debug mode: spawn child with --inspect-brk, real console for CDP events.
    const runnerScript = `'use strict';
process.on('uncaughtException', (err) => {
  console.error('uncaught exception:', err.message);
  if (err.stack) console.error(err.stack);
  setTimeout(() => process.exit(1), 50);
});
process.on('unhandledRejection', (reason) => {
  const msg = reason instanceof Error ? reason.message : String(reason);
  const stack = reason instanceof Error ? reason.stack : undefined;
  console.error('unhandled rejection:', msg);
  if (stack) console.error(stack);
  setTimeout(() => process.exit(1), 50);
});

(async () => {
  try {
    await import('file:///sandbox/user_script.mjs');
    await new Promise(r => setTimeout(r, 50));
    const _result = globalThis._outputSet ? globalThis._outputValue : null;
    process.stdout.write(JSON.stringify(_result));
    // Tagged emission so CDP/Go can separate final result from console output
    console.log('__SANDBOX_RESULT:' + JSON.stringify(_result, null, 2));
    process.exitCode = 0;
  } catch (err) {
    console.error('runtime error:', err.message);
    if (err.stack) console.error(err.stack);
    process.exitCode = 1;
  }
})();
`;

    fs.writeFileSync('/sandbox/setup.js', setupScript);
    fs.writeFileSync('/sandbox/user_script.mjs', userScript);
    fs.writeFileSync('/sandbox/runner.js', runnerScript);

    process.stderr.write('node: starting with --inspect-brk on port 9229...\n');

    const child = spawn('node', [
      '--inspect-brk=0.0.0.0:9229',
      '-r', '/sandbox/setup.js',
      '/sandbox/runner.js',
    ], {
      stdio: ['ignore', 'pipe', 'pipe'],
    });

    child.stdout.on('data', (d) => process.stdout.write(d));
    child.stderr.on('data', (d) => process.stderr.write(d));
    child.on('exit', (exitCode) => process.exit(exitCode ?? 1));
    return;
  }

  // Run mode: redirect console to stderr (stdout reserved for JSON result),
  // load globals, then dynamic-import user code as ESM.
  const origLog = console.log;
  console.log   = (...args) => process.stderr.write(args.map(String).join(' ') + '\n');
  console.error = (...args) => process.stderr.write(args.map(String).join(' ') + '\n');
  console.warn  = (...args) => process.stderr.write(args.map(String).join(' ') + '\n');
  console.info  = (...args) => process.stderr.write(args.map(String).join(' ') + '\n');
  void origLog;

  globalThis.input    = input ?? null;
  globalThis.context  = context ?? {};
  globalThis.params   = params ?? {};
  globalThis._outputValue = undefined;
  globalThis._outputSet   = false;
  globalThis.output = (val) => { globalThis._outputValue = val; globalThis._outputSet = true; };

  fs.writeFileSync('/sandbox/user_script.mjs', userScript);

  // 30s safety net (matches prior vm timeout)
  const watchdog = setTimeout(() => {
    process.stderr.write('runtime error: script exceeded 30s timeout\n');
    process.exit(1);
  }, 30000);
  watchdog.unref();

  try {
    await import('file:///sandbox/user_script.mjs');
    const result = globalThis._outputSet ? globalThis._outputValue : null;
    process.stdout.write(JSON.stringify(result));
    process.exit(0);
  } catch (err) {
    process.stderr.write(`runtime error: ${err.message}\n`);
    if (err.cause) {
      const c = err.cause;
      const causeMsg = c instanceof Error ? `${c.name}: ${c.message}${c.code ? ` (${c.code})` : ''}` : String(c);
      process.stderr.write(`caused by: ${causeMsg}\n`);
      if (c.stack) process.stderr.write(c.stack + '\n');
    }
    if (err.stack) {
      process.stderr.write(err.stack + '\n');
    }
    process.exit(1);
  }
});
