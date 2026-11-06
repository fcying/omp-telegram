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
- Git is needed only for OMP's native autoresearch workflows.
- Network access to Telegram and your model provider.
- A Telegram bot. Ordinary private chats, private-chat topics, and group topics are supported; groups without a topic are not.

Tested with OMP 18.17.0. See the [architecture document](doc/architecture.md) for verification scenarios and limitations.

Only one polling service may use a bot token at a time. Do not run it alongside a webhook or another `getUpdates` consumer.

## Installation

### Download a release

Download the archive for your architecture from [Releases](https://github.com/fcying/omp-telegram/releases). For amd64:

```sh
mkdir -p "$HOME/tool/omp-telegram"
tar -xJf omp-telegram-linux-amd64.txz -C "$HOME/tool/omp-telegram"
```

For arm64, use `omp-telegram-linux-arm64.txz`. Archives contain the binary and LICENSE; omp itself is installed separately. The optional migration Extension is embedded in the binary. Each release includes `SHA256SUMS`.

Development builds are published as the [Development build](https://github.com/fcying/omp-telegram/releases/tag/dev-latest). They are prereleases and may be unstable.

### Build from source

Requires Go 1.27.3 or later (see [go.mod](go.mod)) and [just](https://github.com/casey/just):

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
progress_recent_tools = 5
provider_usage = false

[omp]
binary = "omp"
args = ["${OMP_TELEGRAM_ARGS}"]
args_labels = ["Default"]
migration_extension = false

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

Explicit relative `--config` paths use the launch directory. Relative `storage.data_dir` and `storage.workspace_root` paths always use the real executable directory, not the config file's directory.

Configuration strings support `$VAR` and `${VAR}` except for `[omp.environment]` mode and `allow`/`deny` variable names, which are literal. Use `$$` for a literal dollar sign. Environment references are expanded once after TOML parsing. The default references to `OMP_TELEGRAM_ARGS`, `OMP_TELEGRAM_PROGRESS_MODE`, and `OMP_TELEGRAM_WORKSPACE_ROOT` may be unset. An empty or invalid `telegram.progress_mode` falls back to `summary`. The token and allowlists must be provided. `.env` files are not loaded automatically.
The display-only `omp.args_labels` values are literal and are not environment-expanded.

Both `telegram.allowed_users` and `telegram.allowed_chats` are required. An update is accepted only when both the sender and chat match their allowlists. Private-chat IDs normally match the user ID; group IDs are usually negative numbers.

### Settings

| Setting | Purpose |
| --- | --- |
| `telegram.token` | Telegram bot token. Prefer an environment reference. |
| `telegram.allowed_users`, `telegram.allowed_chats` | Required numeric ID allowlists. Values may be literal integers or comma-separated environment values. |
| `telegram.progress_mode` | Live task progress: `off`, `summary`, or `verbose`. The default reads `OMP_TELEGRAM_PROGRESS_MODE`; empty or invalid values fall back to `summary`. See [Progress UI](#progress-ui). |
| `telegram.progress_recent_tools` | Maximum completed tools retained in verbose Recent history. Default: `5`; range: `1..100`. Display may show fewer to fit Telegram's message limit. |
| `telegram.provider_usage` | Opt in to native `/usage` 5h/7d used percentages in final reply headers. TOML boolean; default `false`. Independent of progress mode. |
| `omp.binary` | OMP executable name found through `PATH`, or an absolute path. It is not a shell command. |
| `omp.args` | Ordered OMP startup presets. Defaults to one optional `OMP_TELEGRAM_ARGS` preset. A legacy scalar string is accepted as one preset. |
| `omp.args_labels` | Optional Telegram picker labels, one per preset; invalid labels fall back to `Preset N`. Labels are visible in the conversation, so never put credentials in them. |
| `omp.migration_extension` | Load the bundled migration Extension on every OMP startup, including resume, for all argument presets. Default: `false`. No separate Extension installation is required. |
| `[omp.environment]` | Optional child environment policy. Default `mode = "denylist"` passes all present and future service variables except names explicitly listed in `deny`. The bundled `config.toml` sets `deny = ["OMP_TELEGRAM_BOT_TOKEN"]`. `mode = "allowlist"` passes only listed, present variables. |
| `storage.data_dir` | Database, lock, incoming/outgoing attachment storage, and generated bridge Extension cache. Default: the executable directory. |
| `storage.workspace_root` | Base directory for `/new <name>`. Defaults to optional `OMP_TELEGRAM_WORKSPACE_ROOT`, then `workspace/` beside the executable. |
| `storage.database_retention_days` | Retention period for terminal bridge messages and eligible attachment caches. Default: `90` days; `0` disables automatic retention cleanup. Workspaces, OMP session data, and saved conversation bindings are not removed. See [Data and Retention](#data-and-retention). |
| `worker.max_workers` | Maximum number of connected OMP processes. Default: `8`; maximum: `64`. |
| `worker.queue_capacity` | Shared per-conversation limit for waiting tasks and unresolved steers. Default: `16`; maximum: `1024`. |
| `worker.idle_timeout` | Time before releasing an otherwise idle OMP process while keeping its session available. Default: `30m`; `0` or `disabled` turns this off. |
| `logging.level` | Global log level: `debug`, `info`, `warn`, or `error`. Default: `info`. |
| `logging.format` | Log format: `text` or `json`. Default: `text`. |
| `[logging.component_levels]` | Optional per-component levels for `daemon`, `bridge`, `rpc`, `telegram`, `store`, and `media`. |

Each `omp.args` item is a complete argument preset, with shell-style quoting but no shell execution. OMP configuration changes only through explicit arguments or user commands, apart from the saved-model recovery fallback described below.

`/args` selects a global preset that survives service restarts. Switching requires an idle conversation with no queued work. It restarts the current open session with the new preset and its native `default` model role; other conversations use it on their next process start. Closed sessions stay closed. A failed restart preserves the selected preset and saved session. Changing preset values resets selection to preset 1; changing labels alone does not.

To explicitly restrict OMP children, replace the existing `[omp.environment]` table with the following. Do not configure `allow` and `deny` together, even if one list is empty.

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
| `/new <name or path>` | Start a fresh session in the selected directory. Replacing a running session requires confirmation. |
| `/new` | Start a fresh session in the previous directory, or `storage.workspace_root` if none was selected. |
| `/stop` | Stop the current task and clear queued tasks while keeping the session available. |
| `/queue` | View running and queued work. Cancel buttons remove individual waiting tasks, not the active task. |
| `/followup <message>` | Queue a separate prompt instead of steering the active task. |
| `/close` | Close the current session while preserving its files and OMP history. |
| `/bindings` | List and manage this chat's saved conversation/session bindings. Icons show logical Open, Closed, or Pending state, not process liveness. See deletion options below. |
| `/bindings old` | List Closed before Open, oldest first in each group; Pending entries last. |
| `/resume` | Choose a saved OMP session from the current workspace, pin/unpin it, or delete it after confirmation. |
| `/resume <session ID>` | Resume the specified native OMP session; OMP may restore its recorded working directory. |
| `/export` | Choose a saved session to export as its original main-session JSONL. Wait until the current session is idle before exporting it. |
| `/export html` | Choose a saved session to export as a standalone HTML viewer. |
| `/export <session ID>` | Export the specified session as the original OMP main-session `.jsonl` without opening a picker. Use `/export html <session ID>` to choose HTML explicitly. |
| `/status` | Show the current workspace, session with its full native ID, model, thinking, fast-mode, context, runtime, and queue status. |
| `/args [preset-number]` | View preset labels or, while idle, select a preset and immediately apply it to this conversation's open session. Other conversations use it on their next process start. |
| `/doctor` | Run safe bridge diagnostics without starting or changing the OMP session. |
| `/name <title>` | Name the current OMP session. It does not rename the Telegram topic. |
| `/model` | Choose a configured OMP model role with buttons. |
| `/model provider/model` | Switch to a specific model while idle. |
| `/thinking` | Choose the thinking level while idle. |
| `/fast [on\|ultra\|off\|status]` | Forward OMP's native fast control, including while a task is running. Bare `/fast` toggles the setting without a picker. |
| `/compact` | Compact the current context immediately while idle, without confirmation. The status message updates with the result and context usage. |
| `/handoff [instructions]` | Run OMP's native handoff while idle with an empty queue, with a 180-second deadline. |
| `/migrate` | Move handoff context to a fresh session in the same workspace. Enable `omp.migration_extension` first. |
| `/review [arguments]` | Run OMP's native `/review` command as a queued task. |
| `/autoresearch [goal\|off\|clear]` | Control native autoresearch when advertised by OMP. See the safety and stopping behavior below. |
| `/lang [en\|zh]` | Show the global Bridge interface language, or switch it to English or Chinese for all chats and topics. |
| `/help` | Show help and the running bridge version (the same version as `-v`). |

On a topic's first session, the bridge attempts to use the workspace name as its title, adding `[thread ID]` for a known duplicate in the same chat. Telegram cannot provide a complete topic list, so unobserved or manually renamed titles may cause missed collisions or unnecessary suffixes. Other topics are not renamed.

A successful model or thinking change can still show `Thinking: n/a` when the configured level is unavailable. This does not undo the change; if the process must close, the reply says that the session was kept.

`/fast` runs immediately, including during a task. OMP determines supported arguments and models. Changes affect subsequent model requests, not one already in flight; its receipt is separate from the task's answer.

Ordinary text starts a task when the conversation is idle. While an OMP task is active, OMP may accept ordinary text as a steer. Use `/followup <message>` to queue a separate prompt instead. Attachments, `/review`, and native commands without dedicated control handling use the conversation queue and run sequentially. Different conversations can run concurrently up to the configured worker capacity.

### Native OMP commands

Bridge commands take precedence over OMP commands; commands addressed to another bot are ignored. Bridge and builtin arguments may use spaces or `:`, for example `/handoff:focus` and `/fast:on`. `/compact` accepts no arguments.

Supported native commands depend on OMP and the current session. Bridge refuses lifecycle-changing commands that it cannot safely keep in sync; common unsupported commands are listed below.

The exact native `/autoresearch` extension has dedicated support. The following native commands are not integrated:

| Command or category | Limitation and alternative |
| --- | --- |
| `/move` | Rejected: native workspace/session moves are not reconciled with the bridge binding. Use `/new` or `/resume`. |
| `/wt`, `/worktree` | Rejected: native worktree creation/switching is not managed by the bridge. Prepare the worktree in OMP or a terminal, then select it with `/new` or `/resume`. |
| `/session delete` | Rejected: use **Delete** in `/resume`, with its confirmation and in-use checks. |
| `/plan`, `/plan-review` | Depend on upstream OMP RPC support. The bridge does not implement their planning workflow; use interactive OMP when RPC support is unavailable. |
| Other extensions and extension aliases | Rejected, including aliases of autoresearch: generic extension session-lifecycle changes are not integrated. Use OMP directly. |
| Advertised commands with unidentified sources | Rejected rather than guessing whether they are safe or forwarding them as ordinary model text. |

The Telegram menu lists bridge commands and available native commands. Telegram allows at most 100 entries with restricted command names; supported commands that do not fit can still be typed manually.

Workspace-specific commands appear after their session connects. Names and descriptions come from OMP. Native command output is delivered separately from task replies.

`/lang en` and `/lang zh` set the Bridge interface language globally for all private chats and topics. Any authorized user can change it, and the setting survives restarts. New Bridge messages use the new language; command menus may take longer to update. Existing messages, OMP replies, native dialogs and descriptions, and user content are not translated. Prompts may use any language.

In a group topic, all allowed users share that topic's OMP session and workspace, including its context and command effects. Telegram group membership is not restricted by the bot's user allowlist: other members who can view the topic can also see bot replies and exported files. Use a trusted group topic for sensitive work.

### Autoresearch

Requires OMP to advertise its native `autoresearch` extension. Send `/autoresearch` commands directly; an attachment caption does not invoke them.

- `/autoresearch <goal>` enables research and starts the native goal.
- `/autoresearch` toggles mode. When enabling a new experiment, it confirms the mode; the next ordinary message supplies the goal.
- `/autoresearch off` disables research and interrupts an active research run; waiting bridge tasks remain queued. Use `/stop` to clear the queue.
- `/autoresearch clear [--keep-tree] [--reset-tree]` requires an idle instance, an empty queue, and confirmation. OMP removes research artifacts and may reset tracked files or delete untracked files. `--keep-tree` skips the worktree reset/clean, but still allows artifact deletion; it is not a backup or a nondestructive mode. Use only where those changes are acceptable.

Two-step usage:

```text
/autoresearch
Reduce this project's startup time without changing its public behavior.
```

Native OMP owns the research loop; see the [native mode instructions](https://github.com/can1357/oh-my-pi/blob/v18.10.3/packages/coding-agent/src/autoresearch/prompt-setup.md). It may create branches, edit files, commit kept experiments, revert discarded experiments, and make repeated model calls. Use a suitable workspace and account for cost and file changes.

The bridge does not lock or isolate workspaces across conversations. OMP sessions using the same workspace can affect the same files; choose separate workspaces or avoid concurrent file-changing tasks. Research is not automatically replayed after a restart; check `/status` and the workspace before resending uncertain work.

### Delete a saved session

In `/resume`, choose **Delete**, then confirm with the red **Delete** button. Sessions in use cannot be deleted; there is no force-delete option.

In `/bindings`, choose **Binding** to forget only that entry while preserving native history, or **Binding+Session** to delete the native session and its bindings and pins across this bot's conversations. If the session file is already missing, only stale Bridge references are removed.

Native session deletion is irreversible. Workspace files and Telegram topics are preserved.

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

If the old working directory recorded in the session no longer exists, OMP may ask you to re-root the session in the current directory. Export is not a project backup: it does not separately collect workspace files, Git state, OMP configuration, credential files, or environment variables. Any such content already recorded in the transcript remains in the export. Synchronize project files separately with `git clone`, `git pull`, `scp`, `rsync`, or another method.

Exported session files may contain sensitive conversation, tool, command, and path data, including prompts, assistant responses, tool calls, tool results, local paths, command output, source snippets, and secrets accidentally present in the transcript. Only send them to trusted Telegram conversations. No additional confirmation is requested; entering `/export` is the explicit user action.

In a group topic, `/export` sends the session file into that topic, not a private chat. Any group member with access to the topic may download it, even if they are not allowed to send commands to the bot.

### Troubleshooting and limitations

| Symptom | Check |
| --- | --- |
| Bot does not respond | Both allowlists, group topics, group privacy settings, and whether another poller or webhook uses the bot. |
| `omp.binary executable not found` | The configured executable and the service process's `PATH`, including omp and its runtime. It may differ from your interactive shell. |
| `/resume` shows no sessions | The selected directory and whether OMP has saved history yet. A new session may have no resumable file. |
| `/compact` fails | Short sessions may have nothing to compact. Check OMP's model configuration if local compaction also fails. |
| Unsupported database schema | Back up the data and use a supported database or fresh data directory; do not change a version number by hand. |

Voice and transcription, automatic topic creation, and arbitrary terminal/editor dialogs are not supported. Some confirmation and selection flows use Telegram buttons, but not every interactive tool approval is available remotely. Automatic approval is never enabled by the bridge.

Very large output or many replies may be truncated with an explicit notice. OMP's native session history is not truncated.

## Progress UI

`telegram.progress_mode` controls live task progress:

- `off` disables progress messages and typing actions.
- `summary` shows active tools, task state, and the current assistant text preview.
- `verbose` additionally shows recent observable tool activity. Tool details are unredacted and may contain credentials or private content visible to everyone who can read the conversation.

Progress appears after about three seconds and includes a **Stop** button for the current task. Its **⚙️** marker shares the first line with available status fields. `/stop` also clears waiting tasks. Progress updates and cleanup are best-effort and do not delay formal replies.

Steer input and completed assistant messages remove the task's older Progress bubbles without restarting OMP. A new preview waits for preceding formal replies to be delivered and appears below the latest input/reply. Remaining temporary Progress bubbles are cleaned up when the task ends; formal replies remain.

Completed assistant messages use **💬** and are sent immediately, even with progress off. Confirmed task success changes only the latest formal status bar to **✅**; cancellation, failure, uncertainty and ongoing research rounds do not. Only the first formal message quotes the original request. Bodies stay unchanged for Telegram Reply, with no body resend or separate completion message.

Status bars show available model, configured thinking, fast-mode, context and speed information, for example `46% (500k)` and `3.8 t/s`. Context is omitted before the first status read completes; `ctx ?` means a completed read could not confirm context usage. Use `/status` for details.

Replies support Markdown and split long text automatically. Large tables may use a plain-text layout; formatting unsupported by Telegram falls back to readable text.

Set `telegram.provider_usage = true` to show native Claude or OpenAI Codex account usage in the final status bar, for example `5h30% 7d65%`. This is independent of progress mode. Unavailable usage is omitted without blocking the answer; account identities and raw reports are not shown.

## Attachments

Send photos or documents with Telegram's attachment button. Add a caption describing what OMP should do; without one, the attachment metadata is still submitted to OMP as a task, but the bridge does not add task instructions. A photo or document album is handled as one task, with up to 10 files and the first non-empty caption.

To receive a file, ask OMP directly, for example, "Send me the report as a file." OMP can return regular files from the current working directory. A message saying that an attachment was queued does not mean it has arrived; check the chat for the actual file.

| Transfer | Limit |
| --- | --- |
| Download from Telegram | 20 MB |
| Send a document | 50 MB |
| Send a photo | 10 MB, JPEG or PNG |

Incoming attachments are stored under `storage.data_dir/attachments/inbox/` and are subject to the configured retention policy. This directory can lie inside a workspace if the configured paths overlap. Copy files you want to keep out of the attachment cache. Large images may be provided as previews or local file paths; image understanding depends on the model, and document reading depends on its tools.

## Telegram reply context

Reply to a Telegram message to include one level of quoted context with ordinary text, attachments, or `/review`. Selected quote text takes precedence; long quotes may be truncated without truncating your current message.

Replied photos and documents are described, not downloaded again. Reply chains are not expanded, and the final response still replies to your current message.

## Sessions and Recovery

Saved OMP sessions survive service restarts. The first prompt or OMP command after restart may take longer while the session reconnects.

Normal resumes keep the saved model. If OMP explicitly reports that it cannot restore that model, the bridge retries once with the selected configuration's `default` model role and reports the fallback. This does not change saved presets or OMP configuration. Other startup failures are not automatically retried.

Interrupted and uncertain work is not automatically replayed. Waiting tasks are cancelled at shutdown or crash; check the conversation and workspace before resending.

`/stop` ends the active task and keeps the session available. `/close` closes it while preserving workspace files and OMP history. If `/new` or `/resume` is interrupted, use `/close` before retrying.

Each conversation has its own session, but sessions using the same workspace share files. Conversations are not filesystem or credential sandboxes.

### Migrate a large session

`/migrate` uses a model-generated handoff summary to start a smaller session in the same workspace, preserving model, thinking and fast-mode settings. The old session remains available. Handoff can take up to 180 seconds. Migration also works for short conversations.

Enable the bundled migration Extension in the existing `[omp]` table, then restart the bridge:

```toml
[omp]
migration_extension = true
```

Migration requires an idle session and empty queue. The bundled Extension needs no separate installation. If you previously loaded it manually with `-e`, remove that argument to avoid duplicate loading. Other Extension commands and aliases remain blocked.

After failure, timeout or interruption, ordinary input stays locked. `/stop` and `/close` stop the runtime but do not unlock migration. Check `/status`, then explicitly recover the source or a known candidate:

```text
/migrate recover old
/migrate recover <full-session-id>
/migrate recover <absolute-session-file>
```

Recovery does not rerun handoff or create another session. If the candidate is unknown, use `/migrate recover old` rather than guessing the newest file. Migration transfers summarized text, not live tools, eval state or all session artifacts.

## Data and Retention

Bridge data is stored in `<storage.data_dir>/omp-telegram.db`, including accepted Telegram input content, outgoing replies, and message metadata. Attachments are stored as files under the same data directory. Treat the database, attachment caches, and their backups as sensitive. `storage.data_dir` defaults to the real executable directory; relative storage paths use that directory rather than the launch directory.

`storage.database_retention_days` controls retention of terminal bridge messages and eligible attachment caches. Default: `90` days. Set it to `0` to disable automatic retention cleanup. Workspace files, OMP session data, and saved conversation bindings are not removed. Retention does not delete delivered Telegram replies.

Before upgrading, stop the service and back up the data directory, workspaces, and OMP's own session storage. The bridge database alone is not a complete backup of OMP conversations.

Authorize trusted users only. The service runs with the filesystem permissions and environment of its OS user, and group members may see prompts and replies even when they cannot control the bot. Keep the bot token private.

## Logging

Logs support `text` and `json`, selected by `logging.format`. `logging.level` sets the global threshold; `[logging.component_levels]` overrides it for `daemon`, `bridge`, `rpc`, `telegram`, `store`, or `media`. See the complete [configuration example](#configuration).

Logs are written to stderr; use your process manager for storage and rotation. Prompts, model replies, credentials, tool payloads, and workspace/session paths are not logged. Chat and thread IDs may still identify conversations, so restrict log access.

## Roadmap

- [ ] **Undo/rewind and redo.** Requires reliable input-to-session tracking and remote rewind support from OMP. Track [#12991](https://github.com/can1357/oh-my-pi/issues/12991), [PR #10026](https://github.com/can1357/oh-my-pi/pull/10026), and [PR #11663](https://github.com/can1357/oh-my-pi/pull/11663).
- [ ] **Reuse the OMP process after Stop.** Requires reliable upstream cancellation. For now, Stop retires the process while preserving its session.
- [ ] **Cancel individual native queued prompts.** Requires reliable upstream cancellation results. `/queue` currently cancels only bridge-pending tasks.
- [ ] **Sub2API account quota.** Requires the deployed fix for [#8473](https://github.com/Wei-Shaw/sub2api/issues/8473) ([PR #8476](https://github.com/Wei-Shaw/sub2api/pull/8476)), quota-read permission, and reliable attribution to the account serving each reply.

## Development

| Command | Purpose |
| --- | --- |
| `just build` | Build the binary. |
| `just fmt` | Format Go code with the version declared in `go.mod`, matching CI. |
| `just test` | Run unit tests without starting or restarting the service. |
| `just check` | Run tests, race checks and `go vet` without starting Telegram polling. |
| `just install` | Install only the binary; configuration and data remain user-managed. |
| `just deploy` | Install and schedule a restart of the existing `supervisord` service after five seconds. Returning does not confirm restart success. |

To check an explicitly selected OMP binary without installing or deploying it:

```sh
just smoke-omp /absolute/path/to/omp
```

This runs real OMP with isolated sessions and a local test provider, without contacting Telegram or a production provider. The [OMP compatibility workflow](.github/workflows/omp-compatibility.yaml) also runs manually and weekly; neither check upgrades the deployed OMP.

## Architecture

See [Architecture](doc/architecture.md) ([中文](doc/architecture.zh.md)) for implementation, failure handling and verification details.
