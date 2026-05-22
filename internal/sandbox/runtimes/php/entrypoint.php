#!/usr/bin/env php
<?php
/**
 * Sandbox entrypoint for PHP execution.
 *
 * Protocol:
 *   stdin  → JSON { code, input, run_input, context, config }
 *   stdout → JSON output (from output() call)
 *   stderr → logs / errors
 *
 * User code runs with $input, $run_input, $context, $config available.
 * Call output($val) to produce a result.
 * echo/print → stderr (stdout reserved for JSON output).
 */

$raw = file_get_contents('php://stdin');
$payload = json_decode($raw, true);

if ($payload === null && json_last_error() !== JSON_ERROR_NONE) {
    fwrite(STDERR, "entrypoint: invalid JSON payload: " . json_last_error_msg() . "\n");
    exit(1);
}

$code      = $payload['code'] ?? '';
$input     = $payload['input'] ?? null;
$run_input = $payload['run_input'] ?? null;
$context   = $payload['context'] ?? [];
$config    = $payload['config'] ?? [];

// output() — sole output mechanism
$_output_value = null;
$_output_set = false;

function output($val) {
    global $_output_value, $_output_set;
    $_output_value = $val;
    $_output_set = true;
}

// Redirect echo/print to stderr via output buffering
ob_start(function ($buffer) {
    fwrite(STDERR, $buffer);
    return '';
}, 1);

try {
    eval($code);
} catch (Throwable $e) {
    fwrite(STDERR, "runtime error: " . $e->getMessage() . "\n");
    fwrite(STDERR, $e->getTraceAsString() . "\n");
    ob_end_clean();
    exit(1);
}

ob_end_clean();

$result = $_output_set ? $_output_value : null;

$json = json_encode($result);
if ($json === false) {
    fwrite(STDERR, "output serialization error: " . json_last_error_msg() . "\n");
    $json = json_encode(strval($result));
}

fwrite(STDOUT, $json);
exit(0);
