# Claude between-tools thinking

For Claude Sonnet 5.5, use `between_tools` to stop thinking before the first response. The model can still send thinking updates between tool calls.

Send a native Claude Messages request:

```json
{
  "model": "claude-sonnet-5-5",
  "max_tokens": 4096,
  "thinking": { "type": "between_tools" },
  "output_config": { "effort": "high" },
  "messages": [{ "role": "user", "content": "Explain this code." }]
}
```

The accepted effort values are `low`, `medium`, and `high`. You can omit effort. Do not add `display`, `budget_tokens`, `block_binding`, or other fields to the `thinking` object. Invalid combinations return a local error. They are not repaired by deletion.

This explicit mode needs a model with between-tools capability. It is not converted to a budget or sent to another provider protocol. Unknown model capabilities do not establish support.

A model suffix still overrides the body. For example, `claude-sonnet-5-5(low)` selects adaptive thinking at low effort instead of between-tools mode. The existing `(none)` suffix is not a between-tools alias. To select this mode, remove the suffix and use the explicit object above.

Keep thinking and redacted blocks in conversation history without changing their opaque strings. Between-tools mode does not establish account affinity or prove that a provider accepted earlier thinking blocks.

See the [Sonnet 5.5 migration guide](https://platform.claude.com/docs/en/models/sonnet-5-5/migration-guide) for provider constraints.
