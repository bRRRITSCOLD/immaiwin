#!/usr/bin/env python3
"""
Sandbox entrypoint for Python execution.

Protocol:
    stdin  → JSON { code, input, context, params }
    stdout → JSON output (value of `output` variable after execution)
    stderr → logs / errors

The user's code runs in a restricted namespace. Available globals:
    input, context, params, print (→ stderr)

Call `output(val)` to produce a result.
"""

import json
import os
import sys
import traceback


def main():
    raw = sys.stdin.read()
    try:
        payload = json.loads(raw)
    except json.JSONDecodeError as e:
        print(f"entrypoint: invalid JSON payload: {e}", file=sys.stderr)
        sys.exit(1)

    code = payload.get("code", "")
    input_data = payload.get("input")
    context = payload.get("context", {})
    params = payload.get("params", {})

    # Redirect print to stderr so stdout stays clean for output JSON
    original_print = print

    def sandbox_print(*args, **kwargs):
        kwargs["file"] = sys.stderr
        original_print(*args, **kwargs)

    # Unified output() function — like Lambda handler pattern
    _output_value = None
    _output_set = False

    def output_fn(val):
        nonlocal _output_value, _output_set
        _output_value = val
        _output_set = True

    # Build execution namespace
    namespace = {
        "input": input_data,
        "context": context,
        "params": params,
        "output": output_fn,
        "print": sandbox_print,
        # Standard safe builtins
        "len": len,
        "range": range,
        "enumerate": enumerate,
        "zip": zip,
        "map": map,
        "filter": filter,
        "sorted": sorted,
        "reversed": reversed,
        "list": list,
        "dict": dict,
        "set": set,
        "tuple": tuple,
        "str": str,
        "int": int,
        "float": float,
        "bool": bool,
        "type": type,
        "isinstance": isinstance,
        "hasattr": hasattr,
        "getattr": getattr,
        "setattr": setattr,
        "min": min,
        "max": max,
        "sum": sum,
        "abs": abs,
        "round": round,
        "any": any,
        "all": all,
        "json": json,
        "True": True,
        "False": False,
        "None": None,
        "__builtins__": {},
    }

    # Debug mode: use debugpy for DAP-based interactive debugging
    if os.environ.get("SANDBOX_DEBUG"):
        script_path = "/sandbox/user_script.py"
        with open(script_path, "w") as f:
            f.write(code)

        try:
            import debugpy
        except ImportError:
            print("debugpy not installed — use debug image", file=sys.stderr)
            sys.exit(1)

        debugpy.listen(("0.0.0.0", 5678))
        print("debugpy: waiting for client on port 5678...", file=sys.stderr)
        debugpy.wait_for_client()
        print("debugpy: client attached", file=sys.stderr)

        try:
            exec(compile(code, "user_script.py", "exec"), namespace)
        except Exception:
            traceback.print_exc(file=sys.stderr)
            sys.exit(1)
    else:
        try:
            exec(compile(code, "sandbox.py", "exec"), namespace)
        except Exception:
            traceback.print_exc(file=sys.stderr)
            sys.exit(1)

    result = _output_value if _output_set else None

    try:
        sys.stdout.write(json.dumps(result))
    except (TypeError, ValueError) as e:
        print(f"output serialization error: {e}", file=sys.stderr)
        sys.stdout.write(json.dumps(str(result)))

    sys.exit(0)


if __name__ == "__main__":
    main()
