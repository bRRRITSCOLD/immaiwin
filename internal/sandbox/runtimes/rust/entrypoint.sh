#!/bin/sh
set -e

# Sandbox entrypoint for Rust execution.
#
# Protocol:
#   stdin  → JSON { code, input, run_input, context, config }
#   stdout → JSON output (from output() call)
#   stderr → logs / errors (eprintln!, cargo output)

# Read payload
cat > /tmp/payload.json

# Write data files for include_str!
jq -c '.input // null'     /tmp/payload.json > /tmp/input.json
jq -c '.run_input // null' /tmp/payload.json > /tmp/run_input.json
jq -c '.context // {}'     /tmp/payload.json > /tmp/context.json
jq -c '.config // {}'      /tmp/payload.json > /tmp/config.json

# Generate main.rs — header
cat > /sandbox/project/src/main.rs << 'HEADER'
use serde_json::{json, Value};
use std::io::Write;

fn output(val: Value) {
    let s = serde_json::to_string(&val).unwrap_or_else(|_| "null".to_string());
    std::io::stdout().write_all(s.as_bytes()).ok();
    std::process::exit(0);
}

fn main() {
    let input: Value = serde_json::from_str(
        include_str!("/tmp/input.json").trim()
    ).unwrap_or(Value::Null);

    let run_input: Value = serde_json::from_str(
        include_str!("/tmp/run_input.json").trim()
    ).unwrap_or(Value::Null);

    let context: Value = serde_json::from_str(
        include_str!("/tmp/context.json").trim()
    ).unwrap_or(Value::Object(Default::default()));

    let config: Value = serde_json::from_str(
        include_str!("/tmp/config.json").trim()
    ).unwrap_or(Value::Object(Default::default()));

    let _ = (&input, &run_input, &context, &config);

HEADER

# Append user code
jq -r '.code' /tmp/payload.json >> /sandbox/project/src/main.rs

# Footer — default null output if output() not called
cat >> /sandbox/project/src/main.rs << 'FOOTER'

    // Default: no output() called → null
    std::io::stdout().write_all(b"null").ok();
}
FOOTER

# Build and run (deps pre-compiled, only user code compiles)
cd /sandbox/project
cargo run --release 2>/dev/stderr
