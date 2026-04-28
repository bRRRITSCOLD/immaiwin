# Hello-world skill tool.
#
# The agent calls this tool with a JSON object matching the manifest's
# `input_schema`. The sandbox runtime exposes that object as the `input`
# global; results are returned by calling `output(...)`.

name = (input or {}).get("name", "world")
formal = (input or {}).get("formal", False)

if formal:
    greeting = f"Good day, {name}. It is a pleasure to make your acquaintance."
else:
    greeting = f"Hey {name}! Nice to see you."

output({
    "greeting": greeting,
    "name": name,
    "formal": formal,
})
