# CLI-backed providers (`kind: cli`)

A provider can run a **local CLI tool** (claude, codex, kiro, a qwen CLI, …) instead
of making an HTTP call. chicco builds the command from a template, runs it once,
buffers the completion, and synthesizes the OpenAI SSE the caller expects — so a CLI
tool appears as just another model behind the same `:41986/v1` endpoint, with the
same cooldown / failover / dashboard behaviour. Adding a new tool is a YAML entry,
no code.

```yaml
  - name: claude-cli
    kind: cli
    command: claude
    args: ["-p", "{{prompt}}", "--model", "{{model}}", "--bare", "--tools", "",
           "--system-prompt", "{{system}}", "--output-format", "json"]
    output: json                 # parse the tool's JSON…
    result_path: result          # …text is here
    tokens_path: usage.output_tokens   # …token count here (optional)
    health_command: ["claude", "--version"]   # exit 0 = healthy
    models: [claude-sonnet-4-6, claude-opus-4-8]
```

| field | meaning |
|---|---|
| `command` / `args` | the tool and its argv; `${VAR}` is env-expanded |
| placeholders | `{{model}}` `{{system}}` `{{user}}` `{{prompt}}` (system+user) `{{output_file}}` |
| `prompt_stdin` | pipe `{{prompt}}` on stdin instead of as an arg |
| `output_file` | read the answer from the temp `{{output_file}}` path (codex) |
| `output` | `text` (default, stdout verbatim) or `json` |
| `result_path` / `tokens_path` | dotted JSON paths when `output: json` |
| `error_path` | dotted JSON path that, when truthy, marks the call failed → cooldown + fail over (e.g. claude's `is_error`) |
| `strip_ansi` | strip colour codes from output (kiro) |
| `health_command` / `health_expect` | health: run a local auth-status command; greys (auth) when it exits non-zero or its output lacks `health_expect` (e.g. `["claude","auth","status"]` + `'"loggedIn": true'`) |
| `credential` | fallback health when no `health_command`: stat a file (missing = needs login) |
| `timeout_seconds` | CLI run timeout (default 120) |

CLI providers need no `api_key` — they authenticate via the tool's own login. A
failed run is treated like a flaky upstream: the provider is cooled down and the
request fails over to the next. chicco reads the failure message:

- an **auth** problem (*not logged in*, *unauthorized*, …) **greys** the provider
  (the same as a bad HTTP key);
- a **usage-limit** hit (*limit reached*, *rate limit*, *quota*, …) cools it down
  **until the window reopens** — chicco parses the reset time from the message
  (*"resets in 2h 30m"*, *"try again in 45 minutes"*, *"resets at 3pm"*) and the
  dashboard's `cd …` countdown then shows **when the next window is available**
  (falling back to 1h when no time is given). These CLIs expose no free "when does
  my quota reset" command, so this is detected from the limit error, not polled.

When a tool doesn't report token usage, chicco estimates it (`≈ len/4`) so the
dashboard bar still moves. See `chicco.yaml` for ready presets.

**Login state in the dashboard.** With a `health_command` (a free local
auth-status check like `claude auth status`), the boot/periodic probe shows the
provider's *real* login state — green only when actually logged in. Without one,
chicco falls back to stat'ing the `credential` file, which only proves the tool is
*set up*, not logged in; such a provider shows green until a real request fails
auth and greys it (see above).

**Tools are text-in / text-out — keep the CLI's own tools off.** chicco flattens
the request to one prompt and reads back plain text; it does not support OpenAI
function-calling (a `tools` array in the request is ignored, with a logged warning).
More importantly, run each CLI in a **no-tools / read-only** mode so it can't edit
files or run commands on the host — any edits would land in chicco's working
directory, and the calling agent expects to apply edits itself from the returned
text. The presets do this where the tool allows it (claude `--bare --tools ""`,
codex `--sandbox read-only`, qwen plain `-p`); kiro has no clean answer-only mode,
so it's the least suitable here.
