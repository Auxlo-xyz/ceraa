# Mini App feature guide

The Ceraa Mini App is the authenticated visual control surface opened from the
Telegram bot. It is a dashboard for observing sessions and managing Ceraa's
user-facing controls. Runs are started from Telegram; the dashboard observes
running work rather than acting as a second web chat.

## Open and authenticate

Open the Mini App from the Telegram bot. The Telegram Web App credential is
used by the page for authenticated requests. If the page is opened without the
Telegram credential, it shows a locked state rather than an empty dashboard.

The page is binary-authoritative and embedded in Ceraa. A released binary does
not need an external HTML file to serve the Mini App. A page source change
requires a rebuild and restart of Ceraa.

## Navigation

The dashboard has four boards:

- **Overview (All)**: service state, current session, transcript, run history,
  token usage, cache telemetry, mounted roots, and session token split.
- **Context & Roots**: the pinned workspace root, mounted project roots, the
  `files/` upload browser, the workspace browser, context compaction, skills,
  and context-related controls.
- **Tools, Permissions & Cron**: tool inventory and permission state, always-allow
  toggles, and scheduled jobs.
- **Agents, Providers & Secrets**: provider profiles, subscription state, model
  catalog, run parameters, and the encrypted user-secret store.

The four boards are the desktop navigation and the mobile capsule navigation.
They refer to the same state, not separate views.

## Sessions and runs

The Overview board can show sessions, open a session, inspect its transcript,
and switch the selected session's inspector between:

- todos
- snapshots
- run history

The run history is durable. Tool progress and live state are observed through
server-sent events and the dashboard refresh path. If a live event stream
becomes stale, reread the session rather than assuming the last visual state is
current.

The dashboard reports token usage as separate input, output, cache-read, and
cache-write values. It also shows cost and cache telemetry where available.
Reasoning is streamed separately while a run is live and is not persisted as a
reopened transcript block.

## Context and files

The Context & Roots board shows the environment-pinned workspace root and the
mounted project roots. A project root must remain inside the configured
workspace boundary. The file browser has separate scopes for:

- `files/`, where Telegram uploads live
- `workspace/`, where agent working files live

The dashboard can show file names, notes, and sizes. It is not a replacement
for the agent's file tools.

The Context board exposes compaction controls and the auto-compaction setting.
Use **Compact Now** when the operator wants to compact the current context
immediately. Auto-compaction controls whether the normal threshold is applied.

## Tools, permissions, and schedules

The Tools board shows the read-only registered tool inventory and whether each
tool is configured as **Always Allow** or **Ask First**. The always-allow list
controls whether an operation needs a human permission request. It does not
make a dangerous tool safe; use it only for tools the operator understands.

The same board manages scheduled jobs. A schedule has a cron expression and a
prompt or maintenance task, and can be deleted from the job card. Verify the
cron expression and target session before adding a recurring job.

## Agents and providers

The Agents board manages provider profiles, active provider selection, model
catalog data, subscriptions, reasoning effort, context limits, output limits,
tool-output limits, concurrency, and the selected agent mode. Inputs that are
marked for persistence are local edits until **Save** is pressed.

The model catalog reports its source and refresh state. Provider credentials and
subscription login flows remain provider-specific. Use the provider controls
rather than putting credentials into the dashboard transcript.

## User secrets

User secrets are managed in the **AES-256 Secret Store** panel on the Agents
board. Choose **+ Add secret**, enter the name and value, and save. Values use
a password field and are never echoed. The dashboard shows names and byte
lengths only. Existing secrets can be deleted from the same panel.

Do not place a secret in a prompt, screenshot, transcript, file, or log.

## What this dashboard does not do

- It does not provide a separate web chat composer.
- It does not replace Telegram for starting runs.
- It does not expose secret values after storage.
- It does not make source-tree edits safe by itself.
- It does not make an old page beside the binary authoritative.

When a display looks stale, first check session refresh and the running binary.
When a UI source change is intended, rebuild and restart before judging the
result.
