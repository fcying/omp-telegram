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

Each ordinary private chat or topic has its own OMP session. Different conversations can work concurrently.

## Requirements

- Linux on amd64 or arm64.
- An omp installation supporting RPC protocol v2 and its runtime, such as Bun. Configure models and authentication for the same user that will run this service.
- Git available on `PATH`, including for physical-workspace admission checks outside Git repositories.
- Network access to Telegram and your model provider.
- A Telegram bot. Ordinary private chats, private-chat topics, and group topics are supported; groups without a topic are not.

Text steering requires OMP >= 18.3.2.

Only one polling service may use a bot token at a time. Do not run it alongside a webhook or another `getUpdates` consumer.

## Installation

### Download a release

Download the archive for your architecture from [Releases](https://github.com/fcying/omp-telegram/releases). For amd64:

```sh
mkdir -p "$HOME/tool/omp-telegram"
tar -xJf omp-telegram-linux-amd64.txz -C "$HOME/tool/omp-telegram"
```

For arm64, use `omp-telegram-linux-arm64.txz`. Archives contain the binary and LICENSE; omp itself is installed separately. Each release includes `SHA256SUMS`.

Development builds are published as the [Development build](https://github.com/fcying/omp-telegram/releases/tag/dev-latest). They are prereleases and may be unstable.

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

[omp.environment]
mode = "denylist"
deny = ["OMP_TELEGRAM_BOT_TOKEN"]

[storage]
data_dir = "."
workspace_root = "${OMP_TELEGRAM_WORKSPACE_ROOT}"
database_retention_days = 90

[worker]
max_workers = 8
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

Configuration strings support `$VAR` and `${VAR}` except for `[omp.environment]` mode and `allow`/`deny` variable names, which are literal. Use `$$` for a literal dollar sign. Environment references are expanded once after TOML parsing. The default references to `OMP_TELEGRAM_ARGS`, `OMP_TELEGRAM_PROGRESS_MODE`, and `OMP_TELEGRAM_WORKSPACE_ROOT` may be unset. An empty or invalid `telegram.progress_mode` falls back to `summary`. The token and allowlists must be provided. `.env` files are not loaded automatically.

Both `telegram.allowed_users` and `telegram.allowed_chats` are required. An update is accepted only when both the sender and chat match their allowlists. Private-chat IDs normally match the user ID; group IDs are usually negative numbers.

### Settings

| Setting | Purpose |
| --- | --- |
| `telegram.token` | Telegram bot token. Prefer an environment reference. |
| `telegram.allowed_users`, `telegram.allowed_chats` | Required numeric ID allowlists. Values may be literal integers or comma-separated environment values. |
| `telegram.progress_mode` | Live task progress: `off`, `summary`, or `verbose`. The default reads `OMP_TELEGRAM_PROGRESS_MODE`; empty or invalid values fall back to `summary`. See [Progress UI](#progress-ui). |
| `omp.binary` | OMP executable name found through `PATH`, or an absolute path. It is not a shell command. |
| `omp.args` | Additional OMP arguments. Defaults to optional `OMP_TELEGRAM_ARGS`; an explicit empty string disables them. |
| `[omp.environment]` | Optional child environment policy. Default `mode = "denylist"` passes all present and future service variables except names explicitly listed in `deny`. The bundled `config.toml` sets `deny = ["OMP_TELEGRAM_BOT_TOKEN"]`. `mode = "allowlist"` passes only listed, present variables. |
| `storage.data_dir` | Database, lock, and incoming/outgoing attachment storage. Default: the executable directory. |
| `storage.workspace_root` | Base directory for `/new <name>`. Defaults to optional `OMP_TELEGRAM_WORKSPACE_ROOT`, then `workspace/` beside the executable. |
| `storage.database_retention_days` | Retention period for terminal bridge records, unfinished session starts, and eligible incoming attachments. Default: `90`; `0` disables automatic retention cleanup. See [Data and Retention](#data-and-retention). |
| `worker.max_workers` | Maximum number of connected OMP processes. Default: `8`; maximum: `64`. |
| `worker.queue_capacity` | Maximum number of waiting tasks per conversation. Default: `16`; maximum: `1024`. |
| `worker.idle_timeout` | Time before releasing an otherwise idle OMP process while keeping its session available. Default: `30m`; `0` or `disabled` turns this off. |
| `logging.level` | Global log level: `debug`, `info`, `warn`, or `error`. Default: `info`. |
| `logging.format` | Log format: `text` or `json`. Default: `text`. |
| `[logging.component_levels]` | Optional per-component levels for `daemon`, `bridge`, `rpc`, `telegram`, `store`, and `media`. |

`omp.args` supports shell-style quoting for argument grouping, but the value is passed directly and is never executed by a shell. OMP configuration overlays remain OMP configuration; the bridge does not modify your models, credentials, tools, or approval policy.

To explicitly restrict OMP children, configure the table like this:

```toml
[omp.environment]
mode = "allowlist"
allow = ["PATH", "HOME", "YOUR_PROVIDER_API_KEY"]
```

`allow` lists existing service variables OMP needs, including provider credentials and runtime variables such as `PATH`. An incomplete list can prevent OMP from working. This limits inherited environment, not filesystem or same-user process access.

To exclude specific variables while passing the rest:

```toml
[omp.environment]
mode = "denylist"
deny = ["OMP_TELEGRAM_BOT_TOKEN", "SOME_API_KEY"]
```

`deny` lists literal variable names excluded from OMP children; bridge-side TOML `${VAR}` expansion still works. Add a custom Telegram token variable here if OMP should not inherit it.

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
| `/new <name or path>` | Start a fresh session in the selected directory. Replacing a running session requires confirmation. A topic's first session sets its title to the workspace name. |
| `/new` | Start a fresh session in the previous directory, or `storage.workspace_root` if none was selected. |
| `/stop` | Stop the current task and clear queued tasks while keeping the session available. |
| `/followup <message>` | Queue a separate text prompt after the active task; if idle, run it as the next ordinary task. |
| `/queue` | View running and queued work. Cancel buttons remove individual waiting tasks, not the active task. |
| `/close` | Close the current session while preserving its files and OMP history. |
| `/bindings` | List this chat's saved conversation/session bindings. Tap an eligible title to forget a closed or idle binding after confirmation; workspace files and OMP history are preserved. |
| `/bindings old` | Show saved bindings from least recently used to most recently used. |
| `/resume` | Choose a saved native OMP session from the current workspace. Includes Pin/Unpin and Delete controls. |
| `/resume <session ID>` | Resume a native OMP session and its original directory. |
| `/export` | Choose a saved session to export as its original main-session JSONL. Wait until the current session is idle before exporting it. |
| `/export html` | Choose a saved session to export as a standalone HTML viewer. |
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
| `/autoresearch [goal\|off\|clear]` | Control native autoresearch when advertised by OMP. See the safety and stopping behavior below. |
| `/help` | Show help and the running bridge version (the same version as `-v`). |

Ordinary text starts a task when idle, or steers the active task without creating a separate final reply. Use `/followup <message>` for a separate task afterward. Attachments, `/review`, and other supported native commands run through the conversation queue. Different conversations can run concurrently up to the configured worker capacity.

### Native OMP commands

Bridge commands take precedence over OMP commands; commands addressed to another bot are ignored. Bridge and builtin arguments may use spaces or `:`, for example `/handoff:focus` and `/fast:on`. `/compact` accepts no arguments.

Other native commands depend on the current OMP session's advertised support. A queued command that becomes unavailable is cancelled, not sent as model text. Once command discovery succeeds, unrecognized slash input, including file paths and typos, is treated as ordinary text. Some local commands produce no Telegram reply; `/plan` and `/plan-review` still require upstream OMP RPC support.

If OMP does not support command discovery, slash input outside the bridge's own commands is cancelled rather than forwarded to OMP. Ordinary text and bridge controls remain available. If discovery is temporarily unavailable, waiting slash input keeps its queue position; use `/queue` and **Cancel** on that item to let later tasks continue.

The exact native `/autoresearch` extension has dedicated support. The following native commands are not integrated:

| Command or category | Limitation and alternative |
| --- | --- |
| `/move` | Rejected: native workspace/session moves are not reconciled with the bridge binding. Use `/new` or `/resume`. |
| `/wt`, `/worktree` | Rejected: native worktree creation/switching is not managed by the bridge. Prepare the worktree in OMP or a terminal, then select it with `/new` or `/resume`. |
| `/session delete` | Rejected: use **Delete** in `/resume`, with its confirmation and in-use checks. |
| `/plan`, `/plan-review` | Depend on upstream OMP RPC support. The bridge does not implement their planning workflow; use interactive OMP when RPC support is unavailable. |
| Other extensions and extension aliases | Rejected, including aliases of autoresearch: generic extension session-lifecycle changes are not integrated. Use OMP directly. |
| Commands with unidentified sources | Rejected rather than guessing whether they are safe or forwarding them as ordinary model text. |

The Telegram command menu keeps bridge commands first and adds discovered executable native names and aliases. All topics in a chat share one menu; execution still checks the current session. Telegram limits each menu to 100 commands with 1-32 lowercase English letters, digits, or underscores per name. Supported commands omitted because of these limits can still be typed manually.

Native menu entries appear after a runtime has discovered its commands. Before discovery, or after all contributing sessions in the chat have been explicitly closed, the menu contains only bridge commands. Menu publication is best-effort; a missing menu entry alone does not mean a supported command cannot be typed directly.

Command parsing, source checks, and completion semantics are described in the [architecture document](doc/architecture.md#native-command-recognition-and-submission).

All commands work in ordinary private chats and topics. Bot menus, buttons, and service messages are in English; prompts may use any language, and model replies are not translated by the bridge.

In a group topic, all allowed users share that topic's OMP session and workspace, including its context and command effects. Telegram group membership is not restricted by the bot's user allowlist: other members who can view the topic can also see bot replies and exported files. Use a trusted group topic for sensitive work.

### Autoresearch

Requires OMP to advertise its native `autoresearch` extension. Send commands directly, not through `/followup` or an attachment:

- `/autoresearch <goal>` enables research and starts the native goal.
- `/autoresearch` toggles mode. Enabling it without an existing experiment may simply ask for the next prompt; that prompt becomes the research task.
- `/autoresearch off` disables mode and interrupts active research, preserving waiting bridge tasks. When mode is already off, an active ordinary task is left running.
- `/autoresearch clear [--keep-tree] [--reset-tree]` requires an idle instance, an empty queue, and confirmation. Native clear removes research artifacts and may reset tracked files and delete untracked files. `--keep-tree` skips the worktree reset, not research-artifact removal.

Starting research does not request extra confirmation. Native OMP may create and check out Git branches, edit files, auto-commit kept experiments, revert discarded experiments, and continue making model calls. Use a suitable workspace and account for ongoing cost.

Research requires exclusive use of its physical Git worktree, or its directory outside Git. Another topic already bound there prevents enabling research or clearing it; while reserved, other topics cannot start or resume there. Separate linked worktrees do not conflict. An unconfirmed reservation survives idle release, `/close`, and service restart: resume its owning OMP session in the same conversation and confirm `/autoresearch off` to release it. Release the reservation before deleting its owning topic; `/close` does not release it, and its binding cannot be forgotten while reserved. This protects bridge-managed sessions, not external programs or arbitrary tool access to other directories.

A queued research goal encountering a workspace conflict stays paused without submitting the native command. Use `/queue` to cancel an unwanted goal before retrying.

If a directory or Git topology change makes existing reservations overlap, other conversations remain available but ordinary work in the conflicting workspace is refused. In each owning conversation, use `/autoresearch off` or `/stop` to confirm disable separately. If an owner's original directory or session file is unavailable, restore it first; the reservation is retained until disable can be confirmed.

Each completed round is delivered durably, but a round ending alone does not finish the research task or release waiting tasks. When native OMP switches mode off naturally, for example at an experiment limit, the bridge confirms no native work remains, completes the research task, and runs waiting tasks. Ordinary text steers research; `/followup`, attachments, and other native commands remain queued. Progress Stop and `/autoresearch off` preserve that queue; `/stop` clears it. Active research is force-interrupted and marked uncertain, then native mode must be confirmed off before preserved work can run. If disabling cannot be confirmed, the runtime is closed and the queue stays paused. Stopping cannot undo file changes or other tool effects.

Research is not automatically replayed after a restart. The bridge reads the OMP session's recorded control mode; OMP 18.8.2 does not expose authoritative effective research mode through RPC. Recorded-on mode can disagree with a branch-disabled native runtime. Explicitly disable research before changing Git branches or sending unrelated work. `/status` distinguishes active research from recorded enabled mode without an active research task; it cannot resolve this upstream limitation.

### Delete a saved session

In `/resume`, choose **Delete**, then confirm with the red **Delete** button. Only unused sessions can be deleted; close an active session or wait for its export or another deletion to finish first. OMP removes the selected session history and artifacts, and the session list refreshes. Workspace files, Telegram bindings, and topics are not deleted. This action cannot be undone.

### Session export

`/export` lists saved OMP sessions in the current workspace and sends the selected native `.jsonl` session file to Telegram. The default format is the original OMP main-session JSONL. `/export html` exports the selected session as a standalone HTML viewer instead.

Both formats export only the main session, not companion or subagent transcripts, and have a 50 MB document limit.

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

In a group topic, `/export` sends the session file into that topic, not a private chat. Any group member with access to the topic may download it, even if they are not allowed to send commands to the bot.

### Troubleshooting and limitations

| Symptom | Check |
| --- | --- |
| Bot does not respond | Both allowlists, group topics, group privacy settings, and whether another poller or webhook uses the bot. |
| `omp executable not found` | The service process's `PATH`, including omp and its runtime. It may differ from your interactive shell. |
| `/resume` shows no sessions | The selected directory and whether OMP has saved history yet. A new session may have no resumable file. |
| `/compact` fails | Short sessions may have nothing to compact. Check OMP's model configuration if local compaction also fails. |
| Unsupported database schema | Back up the data and use a supported database or fresh data directory; do not change a version number by hand. |

Voice and transcription, automatic topic creation, and arbitrary terminal/editor dialogs are not supported. Some confirmation and selection flows use Telegram buttons, but not every interactive tool approval is available remotely. Automatic approval is never enabled by the bridge.

Very large replies may be truncated with an explicit notice. OMP's native session history is not truncated by the bridge.

## Progress UI

`telegram.progress_mode` controls live task progress.

Supported values:

- `off` disables progress messages and typing actions.
- `summary` shows assistant output, active tool names, and task state.
- `verbose` additionally shows recent tool arguments, updates, and results.

Progress for a new task is delayed for approximately three seconds, so short tasks normally send only their final reply. Active tasks include a **Stop** button. It stops the active task; `/stop` also clears tasks already waiting in the conversation, while the button lets them continue after cancellation.

Progress and final replies are separate. Progress is best-effort UI and does not affect durable final-reply delivery.

Stopping cannot undo tool side effects. When stopping requires force-terminating OMP, execution status may remain uncertain and the session may fail to resume. A prolonged idle stall with pending steering can also trigger recovery; uncertain work is never automatically replayed.

**Privacy:** `verbose` does not redact tool arguments or results. They can reveal file paths, commands, source text, process output, credentials, or private files, including to other readers of a group topic. Detail is truncated, not sanitized; deletion after a task cannot retract what was seen. Use `summary` to omit tool payloads or `off` to disable live progress. Model reasoning and whole RPC frames are never displayed.

## Attachments

Send photos or documents with Telegram's attachment button. Add a caption describing what OMP should do; without one, OMP is asked to inspect the attachment. A photo or document album is handled as one task, with up to 10 files and the first non-empty caption.

To receive a file, ask OMP directly, for example, "Send me the report as a file." OMP can return regular files from the current working directory. A message saying that an attachment was queued does not mean it has arrived; check the chat for the actual file.

| Transfer | Limit |
| --- | --- |
| Download from Telegram | 20 MB |
| Send a document | 50 MB |
| Send a photo | 10 MB, JPEG or PNG |

Incoming files are stored privately under `<storage.data_dir>/attachments/inbox/` and are subject to retention cleanup, even if that storage lies inside your workspace. Copy files you need to keep outside the managed attachment directory. Large images may be provided as previews or local file paths; image understanding depends on the model, and document reading depends on its tools. Stopping a task does not delete an attachment already submitted to OMP.

## Telegram reply context

Reply to a Telegram message to include one level of quoted context with ordinary text, attachments, or `/review`. Selected quote text takes precedence; long quotes may be truncated without truncating your current message.

Replied photos and documents are described, not downloaded again. Reply chains are not expanded, and the final response still replies to your current message.

## Sessions and Recovery

Saved sessions survive service restarts. The first prompt or OMP control command reconnects the original session and may take longer.

Uncertain in-flight operations are not automatically replayed.

If `/new` or `/resume` is interrupted, use `/close` to clear the unfinished start before trying again. Retention may eventually clear it automatically without deleting workspace files or OMP history. Check OMP history first if the result was uncertain.

Waiting tasks are cancelled at shutdown or after a crash, not restored. Check the conversation and workspace before deciding to resend a request.

- `/close` keeps a session closed; `/stop` leaves the session available for later prompts.
- If the saved session file or workspace is unavailable, the bridge reports the failure instead of creating a replacement session. Use `/close`, then `/new` or `/resume` as appropriate.
- `worker.idle_timeout` can release an unused OMP process while keeping its session available. The next prompt or OMP control command resumes the same session; a fresh session may remain connected until OMP has saved its history.
- **Before deleting a Telegram topic, confirm `/autoresearch off` there if it owns a research reservation, then send `/close`.** `/close` does not release that reservation, and another conversation cannot take over its ownership. If the topic is already gone, `/bindings` from another conversation in the same chat can close and forget its idle binding only if it has no research reservation; it cannot release or transfer a reservation.

Each conversation has its own session, but sessions that use the same workspace share files. Separate conversations are not filesystem or credential sandboxes.
To forget another conversation's closed or idle binding, open `/bindings`, tap its title, and confirm deletion. Current-conversation, unfinished-start, and research-reserved entries cannot be deleted there. This removes bridge metadata, not workspace files or native OMP history, and does not cancel already queued final replies or attachments.

## Data and Retention

Bridge state is stored in:

`<storage.data_dir>/omp-telegram.db`

`storage.data_dir` defaults to the directory beside the real executable. Relative storage paths use that directory rather than the launch directory. Use absolute paths when data must stay elsewhere.

`storage.database_retention_days` defaults to 90 days. It controls cleanup of terminal bridge records, unfinished starts, and eligible incoming attachments. Completed incoming attachments are kept while their owner session remains recently active or its state cannot be verified; old interrupted downloads can also be cleaned. Keep needed files in your workspace rather than relying on attachment storage.

Set:

```toml
[storage]
database_retention_days = 0
```

to disable automatic cleanup.

Retention cleanup does not remove:

- OMP sessions
- workspaces, including incoming files left there by older versions
- other OMP data

Before upgrading, stop the service and back up the data directory, workspaces, and OMP's own session storage. The bridge database alone is not a complete backup of OMP conversations. Do not delete SQLite `-wal` or `-shm` files, and do not copy only the main database while the service is running.

Authorize trusted users only. The service runs with the filesystem permissions and environment of its OS user, and group members may see prompts and replies even when they cannot control the bot. Keep the bot token private.

## Logging

Logs support `text` and `json`, selected by `logging.format`. `logging.level` sets the global threshold; `[logging.component_levels]` overrides it for `daemon`, `bridge`, `rpc`, `telegram`, `store`, or `media`. See the complete [configuration example](#configuration).

Logs are written to stderr; use your process manager for storage and rotation. Prompts, model replies, credentials, tool payloads, and workspace/session paths are not logged. Chat and thread IDs may still identify conversations, so restrict log access.

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

## Architecture

Implementation and maintenance details belong in [Architecture](doc/architecture.md), also available in [Chinese](doc/architecture.zh.md): command routing, RPC completion, persistence, failure semantics, restart recovery, retention, and logging contracts.
