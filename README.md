![Ceraa](file_00000000c7008210ba34bc23c70dfbb2.png)

# Ceraa

Async-first Go coding agent with a durable Telegram bridge.

## Install

From a release binary (recommended):

```bash
curl -fsSL https://raw.githubusercontent.com/Auxlo-xyz/ceraa/main/install.sh | sh
```

This picks your platform, verifies the checksum, and installs `ceraa`. Then:

```bash
ceraa setup     # Telegram bot token, then provider/model in Telegram onboarding
ceraa doctor    # configuration, provider, Telegram, workspace, storage
ceraa start     # run the Telegram bridge
```

Or build from source (Go 1.27):

```bash
go build -o bin/ceraa .
./bin/ceraa setup
```

`ceraa update` moves an installed copy to a newer release. Releases live at
`https://github.com/Auxlo-xyz/ceraa/releases` with SHA256SUMS beside every
binary.

## What it is

- **Shared agent engine:** CLI and Telegram use the same `harness/coordinator.Runner`.
- **Native tool definitions:** the LLM sees one definition per registered tool, each a terse contract; usage guidance lives in the system prompt rather than in the descriptions, so the schema stays small.
- **Durable execution:** sessions, model steps, tool operations, and recovery checkpoints are persisted as JSON.
- **Concurrent tools:** tool calls from one model response run concurrently with cancellation, permissions, and durable result records.
- **Telegram bridge:** long polling, strict user/chat allowlist, inline keyboards, callback controls, a persistent Menu button, and a Mini App served on loopback.
- **Adaptive workflows:** approval-based skill proposals, evaluation baselines, and durable scheduled jobs.
- **Cost visibility:** provider usage is tracked by input, output, cached input, cache writes, reasoning, and total tokens. OpenRouter pricing is refreshed daily and stale prices are never used silently.
- **Providers:** OpenAI-compatible APIs, with no automatic provider or model default; choose a provider during Telegram onboarding.

## Quick start
```bash
go build -o bin/ceraa .
./bin/ceraa setup
./bin/ceraa doctor
./bin/ceraa start
./bin/ceraa version
./bin/ceraa chat              # interactive terminal session
./bin/ceraa ask "list files in ."
```

(When installed from a release, `bin/ceraa` above is just `ceraa`.)

`setup` asks only for the Telegram bot token. Start the service with `ceraa start`, then choose a provider and complete provider/model/API-key setup in Telegram onboarding. Credentials supplied through environment variables are not copied into the config file. `doctor` checks configuration, provider reachability, Telegram connectivity, workspace access, and writable storage.

**There is no default provider.** Ceraa ships with no provider URL, model, or key, and picks none on your behalf: on first run it asks which provider to use and stores that per provider. `CERAA_API_URL`, `CERAA_MODEL`, and `CERAA_API_KEY` are an optional way to pre-seed one from the environment instead.

Run the Telegram bridge with environment variables (the provider variables are optional; the bot will prompt if they are absent):

```bash
export CERAA_API_URL=https://your-provider.example/v1
export CERAA_API_KEY=your-provider-key
export CERAA_MODEL=provider/model-id
export TELEGRAM_BOT_TOKEN=123:ABC
./bin/ceraa start
```

## Running the service

`ceraa start` runs the Telegram service in the foreground. A second `start` is
refused, not queued: the daemon records its pid, process group and start time in
`ceraa.pid` before its first poll, so two daemons cannot fight over one Telegram
offset.

| command | what it does |
|---|---|
| `ceraa start` | Start the Telegram service in the foreground. Refuses if one is already running. |
| `ceraa stop` | Stop a running service. SIGTERM, 8s grace, then kill. Clears a stale pid file left by a daemon that died without running its cleanup. |
| `ceraa restart` | Restart **in place**: the running daemon execs a new image of itself, so the pid and its environment are preserved. Use this rather than stop-then-start when the daemon holds credentials your shell does not. |
| `ceraa doctor` | Check configuration, provider reachability, Telegram connectivity, workspace access and writable storage. |
| `ceraa version` | Print the version and the licence notice. |

`restart` execing in place is the reason to prefer it over `stop` then `start`:
a separate `start` inherits *your* shell's environment, not the daemon's, so a
credential that lives only in the daemon's env file is lost.

### Keeping it running

`ceraa start` runs in the foreground and exits if the process dies. On a host with
no init system, nothing restarts it, so use the bundled supervisor:

```bash
./scripts/ceraa-supervisor.sh start    # start once and report the pid
./scripts/ceraa-supervisor.sh stop     # stop via `ceraa stop`
setsid nohup ./scripts/ceraa-supervisor.sh watch &   # stay up and restart on death
```

`watch` holds an `flock` so only one watcher runs, reads liveness from the pid file
(never from `pgrep`, which also matches the shell running the grep), and restarts
the daemon when it disappears. Every path it uses is overridable and derives from
`RUNTIME_DIR`, which defaults to `$REPO_ROOT/../../ceraa/runtime`:

```
RUNTIME_DIR/bin/ceraa          the binary it runs
RUNTIME_DIR/run_ceraa.sh       the launcher, which supplies credentials
RUNTIME_DIR/ceraa.log          daemon output
RUNTIME_DIR/supervisor.log     the watcher's own log
RUNTIME_DIR/supervisor.lock    the single-watcher lock
```

Set `RUNTIME_DIR` to relocate all of them at once. `run_ceraa.sh` is the only
thing that reads `TELEGRAM_BOT_TOKEN`, and it is deliberately not tracked by git.
`scripts/run_ceraa.sh` is a reference copy: it reads `RUNTIME_DIR/env.sh` (mode
600, never committed) and defaults to `start` when called with no arguments,
which is how the supervisor invokes it.

### systemd

`ceraa.service` is provided for hosts that have systemd. Its paths assume the
layout above, so adjust `WorkingDirectory`, `ExecStart` and `EnvironmentFile`
before installing it. Where systemd is absent, use the supervisor instead.

## Configuration

Environment variables override `ceraa.json` and `~/.config/ceraa/config.json`.

| Variable | Default | Description |
|-----|---------|-------------|
| `CERAA_API_URL` | none | OpenAI-compatible provider base URL |
| `CERAA_API_KEY` | none | Provider API key |
| `CERAA_MODEL` | none | Model ID |
| `CERAA_CONTEXT_LIMIT` | `236000` | Maximum request context size |
| `CERAA_MAX_OUTPUT_TOKENS` | `8192` | Maximum response tokens |
| `CERAA_SESSION_FILE` | `sessions` | Session JSON directory |
| `CERAA_WORKSPACE_ROOT` | `<installation-root>/workspace` | Root containing selectable Telegram projects; Docker uses `/data/ceraa/workspace` |
| `CERAA_TOOL_OUTPUT_DIR` | OS temporary directory | Directory for saved large tool outputs |
| `CERAA_MAX_TOOL_RESULT_CHARS` | `12000` | Model-visible tool result limit |
| `CERAA_SKILL_FILE` | `~/.config/ceraa/skills` | Versioned skills and proposals |
| `CERAA_EVALUATION_FILE` | `~/.config/ceraa/evaluations.json` | Run metrics and cost baseline |
| `CERAA_SCHEDULER_FILE` | `~/.config/ceraa/scheduler.json` | Durable scheduled jobs |
| `CERAA_MODEL_CATALOG_URL` | `https://openrouter.ai/api/v1/models` | Public model metadata and pricing catalog |
| `CERAA_MODEL_CATALOG_CACHE` | `~/.config/ceraa/model-catalog.json` | Local model catalog cache |
| `CERAA_MODEL_CATALOG_REFRESH_HOURS` | `24` | Maximum catalog age before refresh |
| `read` | tool | Reads a file or lists a directory, and attaches an image file to the model natively, using the active provider's own multimodal support: the image itself, not a transcription of it. Works on any vision-capable model; a model that rejects media is recorded in `~/.config/ceraa/media-support.json` and not asked again. |
| `send_media` | tool | Sends a file the agent produced to the user on Telegram. Telegram only, since it needs a destination. Holds no token or chat id: the model names a file and the configured account decides where it goes. |
| `browser` | tool | Drives a real headless Chromium via go-rod: `open`, `screenshot`, `text`, `html`, `click`, `type`, `eval`, `status`, `close`. Chromium is downloaded once into `~/.config/ceraa/browser`. Not gated by approval prompts. |
| `llm.reasoning` | *(provider default)* | Thinking level: `off`, `minimal`, `low`, `medium`, `high`, `max`. Clamped to what the model supports. Gemini 3 uses `thinkingLevel`; Gemini 2.5 uses `thinkingBudget` |
| `CERAA_TITLE_API_URL` | `https://generativelanguage.googleapis.com/v1beta` | Gemini native endpoint used for automatic session titles |
| `CERAA_TITLE_API_KEY` | none | Gemini key for session titles; `GEMINI_API_KEY` is also accepted |
| `CERAA_TITLE_MODEL` | `gemini-3.5-flash-lite` | Model used for automatic session titles |
| `TELEGRAM_BOT_TOKEN` | none | Telegram bot token |
| `TELEGRAM_ALLOWED_USER_ID` | none | Allowed Telegram user/chat ID |

The OpenRouter catalog endpoint requires no authentication. Pricing is calculated only when the exact model ID has a current, non-provider-variable price. A missing, stale, or uncertain catalog entry produces an explicit unavailable status rather than a guessed cost.

## Telegram controls

Open the control hub with `/menu`, the persistent `📋 Menu` button, or the inline hub shown after `/start`.

The hub provides buttons for status, sessions, provider/model, agent mode, project, tools, todos, context, compaction, snapshots, permissions, skill proposals, evaluations, schedules, and workspace files.

- **Skills:** feedback can create a versioned skill proposal through the agent. Approve or reject proposals with inline buttons. Approved versions can be rolled back from the skill store.
- **Evaluations:** the Eval button shows runs, success rate, average tokens, requests, and known cost per task. Candidate harness changes are guarded against quality drops, unknown costs, and cost-per-task regressions.
- **Schedules:** choose an interval button, then send the task as the next normal message. Jobs survive restarts, are locked against duplicate execution, and send results back to Telegram.

Text commands remain available for compatibility, but the normal user flow is the inline control hub and buttons. Telegram assistant output uses a real preview message with `editMessageText` updates, avoiding Telegram client draft gaps and stale preview banners. Tool progress appears in a readable **Tools used** section with concise redacted arguments, never internal operation IDs.

## Telegram onboarding guide

### 1. Start the bot

Send:

```text
/start
```

The bot shows buttons for:

- **OpenRouter**
- **Poolside**
- **Custom Provider**
- **Lock bot to this account**

If the buttons do not appear, send:

```text
/menu
```

### 2. Choose a provider

Choose a provider button first.

For **OpenRouter** or **Poolside**, send the model ID and API key when asked.

For **Custom Provider**, send:

1. Provider base URL
2. Model ID
3. API key

For **Hermes subscription**, no API key is needed. Ceraa starts a Nous Portal device login, stores the resulting credential in the encrypted secret store, and activates the provider.

For **Codex (ChatGPT)**, no API key is needed either. Ceraa starts a ChatGPT
device login and routes inference to the Codex subscription endpoint. If a
credential is already stored, picking it activates immediately; otherwise the
login starts and the provider activates on approval.

API keys entered during Telegram onboarding are stored in the encrypted secret store.

### 3. Session names

After the first meaningful task in a session, Ceraa automatically asks Gemini for a short descriptive title using `gemini-3.5-flash-lite`. The title is saved with the session and appears in the Sessions menu. Configure the key with `CERAA_TITLE_API_KEY`, `GEMINI_API_KEY`, or `/setsecret gemini_title <key>`.

The Sessions menu also provides inline buttons to open, rename, regenerate, or delete a session. Deleting requires confirmation. Session IDs remain internal. Configure the title model, API key, and test the connection from **Settings → Session title settings**.

### 4. Lock the bot

Tap **Lock bot to this account** after setup. This records your Telegram user ID as the allowlisted user.

Do this before sharing the bot with anyone else.

### 5. Choose a project

Open **Project** from the control hub and choose the folder where the agent should work.

The project is important. The agent should work inside the selected project unless you explicitly choose another path. Telegram will not run the first task until a project is selected. Projects live under `CERAA_WORKSPACE_ROOT`; create or place project directories there before selecting them.

The Project menu lists directories under the workspace root. System files can still be inspected with the read tool and shell commands; the workspace restriction applies to project selection, not a global tool-path sandbox. Search runs through rg from the shell rather than a maintained index, so there is no per-project index to keep current.

### 6. Send your first task

Send a normal message such as:

```text
List the files in this project and explain what the project does.
```

The bot shows progress and sends the final answer.

You do not need to memorize commands. Use the buttons for normal actions.

## Main controls

### Menu

Opens the control hub.

```text
/menu
```

### Status

Shows the current session, model, project, and system status.

```text
/status
```

### New Session

Starts a fresh conversation while keeping saved sessions.

```text
/new
```

### Sessions

Shows previous conversations.

```text
/sessions
```

Open one with the inline Sessions menu. The menu shows the automatic title and message count, with buttons to open, rename, regenerate the title, or delete. Deleting asks for confirmation.

The text command remains available for automation:

```text
/open <session_id>
```

### Messages

Shows recent messages from the current session.

```text
/messages
```

### Project

Shows or changes the folder the agent works in.

```text
/project
```

### Provider

Changes the active provider or model profile.

```text
/model
```

The Model button is the Provider/Model menu. It can select a saved provider, a built-in provider, or a custom provider. Choose Model opens a searchable inline catalog; model buttons select the model directly.

### Agent Mode

Changes how the agent approaches the task.

```text
/agent
```

- **Build** can inspect and change the project.
- **Plan** creates a plan without changing files.

## Context

### Context

Shows how much conversation context is currently in use.

```text
/context
```

### Compaction

Summarizes older conversation history to make room for more work.

```text
/compaction
```

Use:

```text
/compaction now
```

to request compaction on the next run.

### Todos

Shows the current task checklist.

```text
/todos
```

## Tools and files

### Tools

Lists the tools the agent can use.

```text
/tools
```

### Files

Opens the workspace file browser.

```text
/file_browser
```

### Search

The project search index was removed: there is no index to keep current, and
the agent searches with `rg` from the shell instead. The `/search` and
`/index` entries still appear in the command list but have no backend; use the
agent or the shell for search.

### Snapshots

Shows files changed by the agent and allows restoring the latest snapshot.

```text
/snapshots
```

### Revert

Restores the latest snapshot for a file.

```text
/revert <path>
```

## Safety

### Permissions

Controls which tools require approval.

```text
/permission
```

### Abort

Stops the current run.

```text
/abort
```

## Advanced features

### Skills

Shows reusable skill proposals created from user feedback.

```text
/skill
```

A proposal must be approved before it becomes active.

### Evaluation

Shows task success, token usage, and known cost.

```text
/eval
```

### Schedule

Creates recurring or one-time jobs.

```text
/schedule
```

Choose an interval with the inline buttons, then send the task as the next normal message.

### MCP

Shows configured MCP integrations.

```text
/mcp_catalog
```

### Worktree

Shows Git worktrees for the project.

```text
/worktree
```

### Secrets

Stores encrypted integration secrets separately from normal configuration.

```text
/secrets
/setsecret
/delsecret
```

## Advanced commands

These commands are useful but are not required for normal use:

```text
/help
/settings
/ls
/delete
/rename
/open
/index
/snapshots
/revert
/permission
/detach
/attach
/skills_catalog
/command_catalog
/codex-login
/codex-status
```

`detach` stops sending bot updates to the current chat.
`attach` reconnects the chat.

`/codex-login` signs in to ChatGPT/Codex with a device code and stores the
tokens encrypted; `/codex-status` reports whether a credential is held and
whether Codex is the active provider. Device code authorization must be enabled
for the account in ChatGPT under **Codex → Security** first. Choosing
🧠 Codex (ChatGPT) in the provider menu activates the subscription as a provider.

## Architecture

```text
Telegram / CLI
     |
     v
coordinator.Runner
     |                    +----------------------+
     |                    | OpenRouter catalog  |
     |                    | daily price refresh  |
     v                    +----------------------+
sessionstore + approved skills
     |
     v
contextbuilder -> llm.StreamingClient -> provider /chat/completions
     |
     v
operation.LocalManager -> concurrent tools -> durable results
     |
     v
evaluation baseline + scheduler + Telegram callbacks
```

The runner persists the system/setup prefix, conversation, assistant tool calls, tool results, run status, operation state, usage, cost, pricing status, and recovery metadata.

## Packages

| Package | Responsibility |
|---------|----------------|
| `harness/coordinator` | Shared durable runner, tool dispatch, callbacks, and recovery |
| `harness/llm` | OpenAI-compatible streaming, retries, usage parsing, pricing catalog, and cost calculation |
| `harness/sessionstore` | Atomic JSON sessions, titles, operations, runs, usage, and crash recovery |
| `harness/sessionnamer` | Automatic Gemini session-title generation and fallback naming |
| `harness/contextbuilder` | Request assembly, system/context setup, and token budgeting |
| `harness/operation` | Concurrent tool execution and cancellation |
| `harness/tool` | Built-in tools, catalog tools, and skill proposal tools |
| `harness/compaction` | Conversation summarization and tool-output pruning |
| `harness/skills` | Versioned skills, feedback proposals, approval, and rollback |
| `harness/evaluation` | Per-run metrics, baselines, and quality/cost guards |
| `harness/scheduler` | Durable interval/one-time jobs and restart recovery |
| `harness/permission` | Human approval and remembered tool permissions |
| `harness/primitives` | Bounded shell, file, and glob primitives |
| `internal/telegram` | Long polling, inline menus, callbacks, scheduler execution, and notifications |

## Tool behavior

The default registry includes shell, read, write, view image, browser, web fetch, web search, question, todo, and goal tools. Glob, grep and ls were removed because shell already covers them, and the catalog tools went unused; removing them cut the tool schema from about 1,400 tokens to 1,225 per request. Skill-proposal tools are exposed only when the request contains a relevant intent, keeping the normal request schema small.

Large tool results are written to a file and replaced in the model context with a short path/size/tail summary. Web-search context limits are enforced. Snapshot, todo, permission, and operation state remain durable across restarts.

## Docker

The image is a long-polling service. Its Mini App listens on loopback only and reaches the internet through a Cloudflare tunnel rather than a published port, so no port is exposed. Set `CERAA_MINIAPP=0` and `CERAA_MINIAPP_TUNNEL=0` to run without it. It includes CA certificates for provider, Telegram, and web-tool HTTPS requests, runs as a non-root user, and keeps runtime state under `/data/ceraa`.

Build the image:

```bash
docker build -t ceraa .
```

Run interactive setup once and persist the configuration/state volume:

```bash
docker volume create ceraa-data
docker run --rm -it -v ceraa-data:/data/ceraa ceraa setup
```

Start the Telegram service with the project directory mounted at `/workspace`:

```bash
docker run -d \\
  --name ceraa \\
  --restart unless-stopped \\
  -v ceraa-data:/data/ceraa \\
  -v "$PWD:/workspace" \\
  ceraa
```

`/data/ceraa` persists configuration, sessions, skills, evaluations, schedules, model pricing cache, and the encrypted secret store. `/workspace` is the project the agent operates on. Use `docker logs -f ceraa` for process logs. Provider and Telegram environment variables remain supported as overrides; the normal onboarding path configures them from Telegram.

## Development

```bash
make build     # build the single binary
make test      # run tests with the race detector
make check     # run vet and gofmt checks
make fuzz      # run coordinator fuzz tests
make clean     # remove build and runtime artifacts
```

Direct validation commands:

```bash
go test -race ./...
go vet ./...
go build -o bin/ceraa .
```

The system prompt is compiled into the binary from `prompt/system.txt`. It is the
single source used at build time, so changing it requires a rebuild. There is no
prompt environment override: Ceraa performance depends on one controlled prompt
being used consistently. The project is Go-only and uses the short module path
`ceraa`.

## License

PolyForm Noncommercial License 1.0.0. See [LICENSE](LICENSE) for the full terms.

Commercial use is not permitted under this license.
