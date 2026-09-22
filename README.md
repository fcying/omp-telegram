# omp-telegram

English | [中文](README.zh.md)

`omp-telegram` is a Telegram bridge for [Oh My Pi](https://github.com/can1357/oh-my-pi).

It provides persistent Telegram conversations backed by OMP sessions, including:

- session recovery after a service restart
- streaming task progress and Stop controls
- text and attachment delivery
- model, thinking, and fast-mode controls
- durable final-reply delivery
- structured logging

Each ordinary private chat or topic has its own OMP session. Ordinary prompts, attachments, and `/review` run sequentially within that conversation; different conversations can work concurrently.

## Requirements

- Linux on amd64 or arm64.
- An omp installation supporting RPC protocol v2 and its runtime, such as Bun. Configure models and authentication for the same user that will run this service.
- Network access to Telegram and your model provider.
- A Telegram bot. Ordinary private chats, private-chat topics, and group topics are supported; groups without a topic are not.

Only one polling service may use a bot token at a time. Do not run it alongside a webhook or another `getUpdates` consumer.

## Installation

### Download a release

Download the archive for your architecture from [Releases](https://github.com/fcying/omp-telegram/releases). For amd64:

```sh
mkdir -p "$HOME/tool/omp-telegram"
tar -xJf omp-telegram-linux-amd64.txz -C "$HOME/tool/omp-telegram"
```

For arm64, use `omp-telegram-linux-arm64.txz`. Archives contain the binary and LICENSE; omp itself is installed separately. Each release includes `SHA256SUMS`.

Development builds are published as the [Development build](https://github.com/fcying/omp-telegram/releases/tag/dev). They are prereleases and may be unstable.

### Build from source

Requires the Go version in [go.mod](go.mod) and [just](https://github.com/casey/just):

```sh
git clone https://github.com/fcying/omp-telegram.git
cd omp-telegram
just install
```

This installs the binary to `~/tool/omp-telegram/omp-telegram`. Use `just install /your/bin/directory` to choose another directory. Configuration and data are not copied. Without `just`, build with:

```sh
go build -o omp-telegram ./cmd/omp-telegram
```

## Configuration

Copy [config.toml](config.toml) beside the binary, or create a configuration file with the following settings:

```toml
[telegram]
token = "${OMP_TELEGRAM_BOT_TOKEN}"
allowed_users = ["${OMP_TELEGRAM_ALLOWED_USERS}"]
allowed_chats = ["${OMP_TELEGRAM_ALLOWED_CHATS}"]
progress_mode = "$OMP_TELEGRAM_PROGRESS_MODE"

[omp]
binary = "omp"
args = "${OMP_TELEGRAM_ARGS}"

[storage]
data_dir = "."
workspace_root = "${OMP_TELEGRAM_WORKSPACE_ROOT}"
database_retention_days = 90

[worker]
max_workers = 4
queue_capacity = 16
idle_timeout = "30m"

[logging]
level = "info"
format = "text"

[logging.component_levels]
# daemon = "debug"
# bridge = "debug"
# rpc = "debug"
# telegram = "warn"
# store = "debug"
# media = "debug"
```

### Configuration files and environment variables

Configuration is selected in this order:

1. The file passed with `--config <path>` or `-c <path>`.
2. `config.toml` beside the real executable, after resolving symlinks.
3. The embedded defaults when that default file does not exist.

An explicitly selected missing file, unreadable file, or invalid TOML causes startup to fail. Configuration uses the grouped tables shown above; legacy flat fields and mixed layouts are not accepted.

Strings support `$VAR` and `${VAR}`. Use `$$` for a literal dollar sign. Environment references are expanded once after TOML parsing. The default references to `OMP_TELEGRAM_ARGS`, `OMP_TELEGRAM_PROGRESS_MODE`, and `OMP_TELEGRAM_WORKSPACE_ROOT` may be unset. An empty or invalid `telegram.progress_mode` falls back to `summary`. The token and allowlists must be provided. `.env` files are not loaded automatically.

Both `telegram.allowed_users` and `telegram.allowed_chats` are required. An update is accepted only when both the sender and chat match their allowlists. Private-chat IDs normally match the user ID; group IDs are usually negative numbers.

### Settings

| Setting | Purpose |
| --- | --- |
| `telegram.token` | Telegram bot token. Prefer an environment reference. |
| `telegram.allowed_users`, `telegram.allowed_chats` | Required numeric ID allowlists. Values may be literal integers or comma-separated environment values. |
| `telegram.progress_mode` | Live task progress: `off`, `summary`, or `verbose`. The default reads `OMP_TELEGRAM_PROGRESS_MODE`; empty or invalid values fall back to `summary`. See [Progress UI](#progress-ui). |
| `omp.binary` | OMP executable name found through `PATH`, or an absolute path. It is not a shell command. |
| `omp.args` | Additional OMP arguments. Defaults to optional `OMP_TELEGRAM_ARGS`; an explicit empty string disables them. |
| `storage.data_dir` | Database, lock, and outgoing attachment storage. Default: the executable directory. |
| `storage.workspace_root` | Base directory for `/new <name>`. Defaults to optional `OMP_TELEGRAM_WORKSPACE_ROOT`, then `workspace/` beside the executable. |
| `storage.database_retention_days` | How long terminal bridge message metadata is retained. Default: `90`; `0` disables automatic cleanup. |
| `worker.max_workers` | Maximum number of connected OMP processes. Default: `4`; maximum: `64`. |
| `worker.queue_capacity` | Maximum number of waiting tasks per conversation. Default: `16`; maximum: `1024`. |
| `worker.idle_timeout` | Time before releasing an otherwise idle OMP process while keeping its session available. Default: `30m`; `0` or `disabled` turns this off. |
| `logging.level` | Global log level: `debug`, `info`, `warn`, or `error`. Default: `info`. |
| `logging.format` | Log format: `text` or `json`. Default: `text`. |
| `[logging.component_levels]` | Optional per-component levels for `daemon`, `bridge`, `rpc`, `telegram`, `store`, and `media`. |

`omp.args` supports shell-style quoting for argument grouping, but the value is passed directly and is never executed by a shell. OMP configuration overlays remain OMP configuration; the bridge does not modify your models, credentials, tools, or approval policy.

After changing configuration or its environment, restart the service. For background operation, use a process manager and explicitly provide the environment and a `PATH` containing both omp and its runtime.

## Usage

### Start the service

1. Create a bot with [@BotFather](https://t.me/BotFather) and keep its token private.
2. For a private chat, open the bot and press Start. For topics, enable private-chat topic mode or add the bot to a topic-enabled group. Create topics yourself; the service does not create them.
3. In groups, ensure the bot receives ordinary messages, not only commands. BotFather's `/setprivacy` setting or suitable administrator permissions may be required.
4. Obtain your numeric user and chat IDs using a Bot API client you control. Do not give the bot token to an ID lookup website.

Set the variables referenced by the configuration, then validate and start the service:

```sh
export OMP_TELEGRAM_BOT_TOKEN='your-bot-token'
export OMP_TELEGRAM_ALLOWED_USERS=123456789
export OMP_TELEGRAM_ALLOWED_CHATS=123456789
# Optional: off, summary, or verbose.
export OMP_TELEGRAM_PROGRESS_MODE=summary

~/tool/omp-telegram/omp-telegram --check
~/tool/omp-telegram/omp-telegram
```

`--check` validates local configuration, finds the omp executable, and creates configured data and workspace directories. It does not test Telegram or model authentication. The final command runs in the foreground; Ctrl-C stops it.

### Start a session

Use `/new` with a project name or path:

```text
/new demo
/new /home/you/projects/my-app
```

A named workspace is created when it does not exist. Existing files are not copied or cleared. Plain `/new` uses the previous workspace for this conversation, or `storage.workspace_root` when no workspace has been selected. Wait for the ready message before sending ordinary text.

### Conversation commands

| Command | What it does |
| --- | --- |
| `/new <name or path>` | Start a fresh session in the selected directory. Replacing a running session requires confirmation. On an unbound Telegram topic, the Telegram topic title is set to the resolved workspace basename; later replacements do not rename it. |
| `/new` | Start a fresh session in the previous directory, or `storage.workspace_root` if none was selected. |
| `/stop` | Stop the current task and clear queued tasks while keeping the session open. A released session only clears queued tasks. |
| `/queue` | Show the current conversation's running state and pending bridge queue. Each pending task has an independent Cancel button; it never cancels the active task. The queue is runtime-only; daemon shutdown cancels pending tasks and does not restore them. |
| `/close` | Close the current session while preserving its files and OMP history. |
| `/bindings` | List saved conversation/session bindings for this Telegram chat, including native session names when available. Pending, open, and current entries cannot be deleted; a closed entry from another topic can delete bridge metadata without deleting workspace or native OMP history. |
| `/resume` | Choose a saved native OMP session from the current workspace. Pinned sessions for this conversation and workspace appear first; each row has a Pin or Unpin button. |
| `/resume <session ID>` | Resume a native OMP session and its original directory. |
| `/export` | Open a read-only picker for native OMP session export from the current workspace. The default format is the original main-session JSONL; the source is copied to the private attachment outbox, its filename is retained after sanitization, and the 50 MB document limit applies. Selecting the current session is rejected while its task or queue is active; run `/export` again after it becomes idle. |
| `/export html` | Open the export picker for native OMP HTML rendering. The bridge first snapshots only the selected main-session JSONL into its private spool, then invokes the native exporter from that stable snapshot; companion or subagent transcripts are not included. The result is stored as `omp-session-<short-id>.html`; exporter timeout is 30 seconds and output growth is bounded by the 50 MB document limit. |
| `/export <session ID>` | Export the specified session as the original OMP main-session `.jsonl` without opening a picker. Use `/export html <session ID>` to choose HTML explicitly. |
| `/status` | Show workspace, session, model, thinking, fast mode, context, activity, queue, and speed. |
| `/doctor` | Run safe, asynchronous bridge diagnostics without starting or changing the OMP session. |
| `/name <title>` | Name the current OMP session. It does not rename the Telegram topic. |
| `/model` | Choose a configured OMP model role with buttons. |
| `/model provider/model` | Switch to a specific model while idle. |
| `/thinking` | Choose the thinking level while idle. |
| `/fast [on\|off\|status]` | Choose fast mode, explicitly enable or disable it, or inspect its status. |
| `/compact` | Compact the current context while idle, after confirmation. |
| `/handoff [instructions]` | Run OMP's native handoff while idle with an empty queue. |
| `/review [arguments]` | Run OMP's native `/review` command as a queued task. |
| `/help` | Show help. |

Ordinary text, attachments, and `/review` are queued per conversation and run sequentially. A message sent while another task is running waits in that conversation; it does not interrupt the active task. Different conversations can run concurrently up to the configured worker capacity.

`/doctor` checks runtime configuration, Telegram `getMe`, SQLite health, data-directory write access, the configured OMP binary, the current workspace and saved session, runtime state, uncertain inbox/outbox records, and free disk space. It returns fixed safe summaries only; it never includes tokens, headers, prompts, raw RPC state, or full local paths. If conversation state changes while checks run, the result is discarded.

All commands work in ordinary private chats and topics. Bot menus, buttons, and service messages are in English; prompts may use any language, and model replies are not translated by the bridge.

### Session export

`/export` lists saved OMP sessions in the current workspace and sends the selected native `.jsonl` session file to Telegram. The default format is the original OMP main-session JSONL. `/export html` exports the selected session as a standalone HTML viewer instead.

Direct forms are also supported:

```text
/export <session-id>
/export html <session-id>
```

To use a native JSONL export on another computer, prepare the corresponding source directory, download the file, and run this in the target project directory:

```sh
omp --resume /path/to/session.jsonl
```

If the old working directory recorded in the session no longer exists, OMP may ask you to re-root the session in the current directory. Session export does not include workspace source files, the source tree, Git state, uncommitted files, credentials, OMP configuration, or the shell environment. Synchronize project files separately with `git clone`, `git pull`, `scp`, `rsync`, or another method.

Exported session files may contain sensitive conversation, tool, command, and path data, including prompts, assistant responses, tool calls, tool results, local paths, command output, source snippets, and secrets accidentally present in the transcript. Only send them to trusted Telegram conversations. No additional confirmation is requested; entering `/export` is the explicit user action.

### Troubleshooting and limitations

| Symptom | Check |
| --- | --- |
| Bot does not respond | Both allowlists, group topics, group privacy settings, and whether another poller or webhook uses the bot. |
| `omp executable not found` | The service process's `PATH`, including omp and its runtime. It may differ from your interactive shell. |
| `/resume` shows no sessions | The selected directory and whether OMP has saved history yet. A new session may have no resumable file. |
| `/compact` fails | Short sessions may have nothing to compact. Check OMP's model configuration if local compaction also fails. |
| Unsupported database schema | Back up the data and use a supported database or fresh data directory; do not change a version number by hand. |

Voice and transcription, automatic topic creation, and arbitrary terminal/editor dialogs are not supported. Some confirmation and selection flows use Telegram buttons, but not every interactive tool approval is available remotely. Automatic approval is never enabled by the bridge.

## Progress UI

`telegram.progress_mode` controls the live task status message.

Supported values:

- `off` disables progress messages and typing actions.
- `summary` shows assistant output, active tool names, and task state.
- `verbose` also shows recent observable tool activity.

Progress for a new task is delayed for approximately three seconds, so short tasks normally send only their final reply. Active tasks include a **Stop** button. It stops the active task; `/stop` also clears tasks already waiting in the conversation, while the button lets them continue after cancellation.

`/queue` only cancels the selected pending bridge task. It does not manage OMP's native queue, reorder work, provide an active-task Stop button, or persist pending tasks across daemon shutdown.

Progress is best-effort UI and does not affect durable final-reply delivery.

Progress does not display model reasoning, raw tool arguments or results, command text, or process output.

## Attachments

Send photos or documents with Telegram's attachment button. Add a caption describing what OMP should do. Without a caption, OMP is asked to inspect the attachment. Photo and document albums are aggregated into one OMP task; the first message reserves one bridge queue slot and later members join it instead of creating separate prompts.

Album members are ordered by Telegram message ID before download. An album accepts at most 10 members. The first non-empty caption is used once; if none is present, the prompt uses `Please inspect the attached files.`. A reply context from the first member that has one is added once, and the final response replies to the first album message. Preparation is all-or-nothing: a failed member download removes the whole incoming directory, removes the owner task, and emits one album failure notice. If collection is canceled, rejected, or sealed, later members for the same media group are consumed without recreating a task during a short suppression window.

To receive a file, ask OMP directly, for example, "Send me the report as a file." OMP can return regular files from the current working directory. A message saying that an attachment was queued does not mean it has arrived; check the chat for the actual file.

| Transfer | Limit |
| --- | --- |
| Download from Telegram | 20 MB |
| Send a document | 50 MB |
| Send a photo | 10 MB, JPEG or PNG |

Incoming files are stored under `.telegram/incoming/` in the selected workspace. Large images may be represented by a preview or a local file path. Image understanding depends on the selected model, and document reading depends on its available tools. Stopping a task does not delete an attachment already submitted to OMP.

Outgoing attachments are copied into a private delivery snapshot before being queued for Telegram delivery, so later changes to the original file do not change the queued payload.

## Telegram reply context

Replying to a previous Telegram message adds one level of quoted context to ordinary text, attachments, and `/review` prompts. A non-empty Telegram quote is preferred. Otherwise the bridge uses the replied text, a photo marker, a document marker with its filename, or a caption-only message. Unsupported replied messages are represented explicitly and do not block the current prompt.

The context identifies the replied sender as `From: bot` or `From: user`. Replied photos and documents are described only; the bridge never downloads or re-imports historical attachments for this feature, and it does not expand a reply chain. The final Telegram response still replies to the current user message; reply context is input to OMP only.

Reply context is capped at 3000 UTF-16 code units and is marked with `...[truncated]` when needed. The current user message is never truncated. `/queue` previews the current user text or attachment caption, not the synthetic OMP wrapper.

Reply context is derived only from the current Update's decoded `ReplyToMessage` and `Quote`. The original update remains in `inbox.raw`; no Telegram history lookup, extra history API request, or reply-context database column is used.

## Sessions and Recovery

Committed sessions survive daemon restarts.

Uncertain in-flight operations are not automatically replayed.

Tasks that were still waiting when the daemon stopped are cancelled. Check the conversation and workspace before deciding to resend a request.

- `/close` keeps a session closed; `/stop` leaves the session available for later prompts.
- If the saved OMP session file or workspace is unavailable at startup, including a fresh session with no persisted history, recovery is skipped, the binding is kept closed, and the bridge tells you to use `/new`; it never creates a replacement session. For other session-switching failures, use `/close` followed by `/new` or `/resume`.
- `worker.idle_timeout` may release an unused OMP process without closing the session, but only after OMP has written a recoverable session file. A fresh native session may remain connected until then. The next prompt or OMP control command resumes the same session.
- **Send `/close` before deleting a Telegram topic.** Deleting a topic does not automatically stop its OMP session.

Each conversation has its own session, but sessions that use the same workspace share files. Separate conversations are not filesystem or credential sandboxes.
`/bindings` lists the saved bindings for the current Telegram chat. Pending, open, and current-conversation entries have disabled delete buttons. A closed binding from another topic can be forgotten after confirmation; this removes only bridge metadata and history snapshots, not the workspace or native OMP session history.

## Data and Retention

Bridge state is stored in:

`<storage.data_dir>/omp-telegram.db`

`storage.data_dir` defaults to the directory beside the real executable. Relative storage paths use that directory rather than the launch directory. Use absolute paths when data must stay elsewhere.

Terminal Telegram bridge records are retained for `storage.database_retention_days`, which defaults to 90 days.

Set:

```toml
[storage]
database_retention_days = 0
```

to disable automatic cleanup.

Retention cleanup does not remove:

- OMP sessions
- workspaces
- other OMP data

Before upgrading, stop the service and back up the data directory, workspaces, and OMP's own session storage. The bridge database alone is not a complete backup of OMP conversations. Do not delete SQLite `-wal` or `-shm` files, and do not copy only the main database while the service is running.

Authorize trusted users only. The service runs with the filesystem permissions and environment of its OS user, and group members may see prompts and replies even when they cannot control the bot. Keep the bot token private.

## Logging

Structured logging supports:

- `text`
- `json`

Global log level:

```toml
[logging]
level = "info"
format = "text"
```

Optional per-component levels:

```toml
[logging.component_levels]
bridge = "debug"
rpc = "debug"
telegram = "info"
```

Supported components:

- `daemon`
- `bridge`
- `rpc`
- `telegram`
- `store`
- `media`

Logs intentionally avoid raw prompts, model replies, Telegram credentials, raw RPC frames, API response bodies, tool arguments, workspace or session paths, stdout, and stderr. Restrict access to logs because chat and thread IDs may still identify conversations.

## Development

```sh
just build
just test
just check
just install
just deploy
```

- `just build` builds the binary.
- `just test` runs unit tests without starting or restarting a service.
- `just check` runs tests, race checks, and `go vet` without starting Telegram polling.
- `just install` installs only the binary; configuration and data remain user-managed.
- `just deploy` installs the binary and restarts the existing supervised daemon.

## Roadmap

No bridge-side feature is currently listed here.

### Waiting for upstream OMP support

- Reliable active-turn steering
- Native queue clear / atomic abort-and-clear

## Architecture

For implementation details and failure semantics, see:

- [Architecture](doc/architecture.md)

The architecture document cover:

- inbox / outbox durability
- Telegram delivery uncertainty
- progress-message persistence
- crash and restart recovery
- session lifecycle
- watchdog behavior
- database retention
- attachment snapshots
- structured logging contracts
