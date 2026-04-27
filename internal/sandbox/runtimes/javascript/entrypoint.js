#!/usr/bin/env node
/**
 * Sandbox entrypoint for JavaScript/TypeScript execution.
 *
 * Protocol:
 *   stdin  → JSON { code, input, context, params }
 *   stdout → JSON output (result of user code)
 *   stderr → logs / errors
 *
 * The user's code is wrapped in an async IIFE. The last expression
 * Call `output(val)` to produce result. Globals available:
 *   input, context, params, output, console (→ stderr)
 */
'use strict';

const { runInNewContext } = require('vm');
const { createRequire } = require('module');

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

  // Redirect console to stderr so stdout stays clean for output JSON
  const sandboxConsole = {
    log:   (...args) => process.stderr.write(args.map(String).join(' ') + '\n'),
    error: (...args) => process.stderr.write(args.map(String).join(' ') + '\n'),
    warn:  (...args) => process.stderr.write(args.map(String).join(' ') + '\n'),
    info:  (...args) => process.stderr.write(args.map(String).join(' ') + '\n'),
  };

  // output() — sole output mechanism
  let _outputValue = undefined;
  let _outputSet = false;

  // Build sandbox context
  // createRequire from /sandbox/ so require('cheerio') finds /sandbox/node_modules
  const sandboxRequire = createRequire('/sandbox/');

  const sandbox = {
    input:    input ?? null,
    context:  context ?? {},
    params:   params ?? {},
    console:  sandboxConsole,
    output:   (val) => { _outputValue = val; _outputSet = true; },
    require:  sandboxRequire,
    JSON,
    Array,
    Object,
    String,
    Number,
    Boolean,
    Date,
    Math,
    RegExp,
    Map,
    Set,
    Promise,
    parseInt,
    parseFloat,
    isNaN,
    isFinite,
    encodeURIComponent,
    decodeURIComponent,
    encodeURI,
    decodeURI,
    setTimeout,
    clearTimeout,
    setInterval,
    clearInterval,
  };

  // Debug mode: write user code to its own file (line numbers match 1:1)
  // Globals injected via -r preload so user_script.js has zero offset
  if (process.env.SANDBOX_DEBUG) {
    const fs = require('fs');
    const { spawn } = require('child_process');

    // Setup script: sets globals via globalThis, loaded via -r flag
    // NOTE: do NOT override console — real console.log must fire so CDP
    // Runtime.consoleAPICalled events reach the debug UI console panel.
    const setupScript = `'use strict';
globalThis.input = ${JSON.stringify(input ?? null)};
globalThis.context = ${JSON.stringify(context ?? {})};
globalThis.params = ${JSON.stringify(params ?? {})};
globalThis._outputValue = undefined;
globalThis._outputSet = false;
globalThis.output = (val) => { globalThis._outputValue = val; globalThis._outputSet = true; };
`;

    // User script: pure user code, line 1 = user line 1
    const userScript = code;

    // Wrapper that runs user code and handles output
    // Uses require() so V8 treats user_script.js as its own script with correct line numbers.
    // urlRegex breakpoints (set before Start) resolve when require() parses user_script.js.
    const runnerScript = `'use strict';
// Catch async errors that escape the try/catch (unhandled rejections, uncaught exceptions)
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
    require('/sandbox/user_script.js');
    // Give async code a tick to finish
    await new Promise(r => setTimeout(r, 50));
    const _result = globalThis._outputSet ? globalThis._outputValue : null;
    process.stdout.write(JSON.stringify(_result));
    // Emit tagged output so CDP/Go can distinguish final result from regular console messages
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
    fs.writeFileSync('/sandbox/user_script.js', userScript);
    fs.writeFileSync('/sandbox/runner.js', runnerScript);

    process.stderr.write('node: starting with --inspect-brk on port 9229...\\n');

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

  // Normal (non-debug) mode
  // Wrap in async IIFE so `return` and `await` work
  const wrapped = `(async function(input) { ${code} })(input)`;

  try {
    await runInNewContext(wrapped, sandbox, {
      filename: 'sandbox.js',
      timeout: 30000, // 30s vm timeout as safety net
    });

    process.stdout.write(JSON.stringify(_outputSet ? _outputValue : null));
    process.exit(0);
  } catch (err) {
    process.stderr.write(`runtime error: ${err.message}\n`);
    if (err.stack) {
      process.stderr.write(err.stack + '\n');
    }
    process.exit(1);
  }
});
