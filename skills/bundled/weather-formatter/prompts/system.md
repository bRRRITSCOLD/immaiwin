When the workflow needs a human-readable weather summary, prefer
`weather_formatter__format_weather` over composing the sentence yourself.
Pass the raw weather line and the city; pick `style: "formal"` or
`"terse"` if the surrounding context implies it (defaults to friendly).
The tool returns `{ message, city, temp_f, style, raw }` — forward
`message` to whatever publishes the summary.
