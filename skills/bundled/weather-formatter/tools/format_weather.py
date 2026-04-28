# weather-formatter / format_weather
#
# Pure-logic tool: takes a raw wttr.in-style line and a city, returns a
# friendly message keyed off the detected conditions. No network, no
# secrets — safe to enable on any agent.

import re

city = (input or {}).get("city", "Unknown")
raw = (input or {}).get("raw", "")

# Author-bound config lives in `params` under a namespaced key
# `<sanitized_slug>__<config_name>` (see internal/workflow/agent_skills.go).
# Per-call `style` arg from the LLM wins; falls back to the author default.
_p = params or {}
_default_style = _p.get("weather_formatter__default_style", "friendly")
_temp_unit = _p.get("weather_formatter__temp_unit", "F")

style = ((input or {}).get("style") or _default_style).lower()
unit_suffix = f"°{_temp_unit}"

# Regex captures the first signed integer that's followed by `°<unit>`.
# Tolerates "+54°F", "54°F", "-3°C", and embedded whitespace from wttr.in.
_temp_pattern = re.compile(rf"([+-]?\d+)\s*°\s*{re.escape(_temp_unit)}")
_match = _temp_pattern.search(raw)
temp_value = int(_match.group(1)) if _match else None

# Cheap condition keywords. Order matters — first match wins.
condition_phrases = [
    ("snow",     "bundle up — snow on the menu"),
    ("rain",     "grab an umbrella"),
    ("drizzle",  "expect a light drizzle"),
    ("thunder",  "stay safe — thunderstorms rolling through"),
    ("fog",      "drive carefully — visibility is reduced"),
    ("cloud",    "partly cloudy skies"),
    ("clear",    "clear skies"),
    ("sunny",    "sunny and bright"),
    ("overcast", "overcast and grey"),
]
condition_advice = "conditions worth checking"
lower_raw = raw.lower()
for kw, phrase in condition_phrases:
    if kw in lower_raw:
        condition_advice = phrase
        break

# Temperature thresholds use F-scale heuristics; tweak if temp_unit=C is
# the typical workflow input.
if temp_value is None:
    temp_phrase = "no temperature reading available"
elif temp_value >= 85:
    temp_phrase = f"a hot {temp_value}{unit_suffix}"
elif temp_value >= 70:
    temp_phrase = f"a warm {temp_value}{unit_suffix}"
elif temp_value >= 50:
    temp_phrase = f"a mild {temp_value}{unit_suffix}"
elif temp_value >= 32:
    temp_phrase = f"a chilly {temp_value}{unit_suffix}"
else:
    temp_phrase = f"a freezing {temp_value}{unit_suffix}"

if style == "formal":
    message = f"The current conditions in {city} are reported as {temp_phrase} with {condition_advice}."
elif style == "terse":
    message = f"{city}: {temp_phrase}, {condition_advice}."
else:
    message = f"Hey, weather watch for {city}! It's {temp_phrase} out there, and {condition_advice}."

output({
    "message": message,
    "city": city,
    "temp_f": temp_value if _temp_unit == "F" else None,
    "temp_value": temp_value,
    "temp_unit": _temp_unit,
    "style": style,
    "raw": raw,
})
