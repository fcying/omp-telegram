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
final_actions = []

[telegram.status_bar]
progress = ["status", "thinking", "model", "fast", "context", "speed"]
intermediate = ["status", "thinking", "model", "fast", "context", "speed"]
final = ["status", "thinking", "model", "fast", "context", "speed"]

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

An explicitly selected missing file, unreadable file, invalid TOML or invalid known setting causes startup to fail. Unknown keys and tables, including old flat fields, are ignored without migration or environment expansion. Startup and `--check` emit ERROR logs with each ignored item's line and column, never its raw key or value.

Explicit relative `--config` paths use the launch directory. Relative `storage.data_dir` and `storage.workspace_root` paths always use the real executable directory, not the config file's directory.

Configuration strings support `$VAR` and `${VAR}` except for `[omp.environment]` mode and `allow`/`deny` variable names, which are literal. Use `$$` for a literal dollar sign. Environment references are expanded once after TOML parsing. The default references to `OMP_TELEGRAM_ARGS`, `OMP_TELEGRAM_PROGRESS_MODE`, and `OMP_TELEGRAM_WORKSPACE_ROOT` may be unset. An empty or invalid `telegram.progress_mode` falls back to `summary`. The token and allowlists must be provided. `.env` files are not loaded automatically.
The display-only `omp.args_labels`, `telegram.status_bar` fields, and `telegram.final_actions` labels/commands are literal and are not environment-expanded.

Both `telegram.allowed_users` and `telegram.allowed_chats` are required. An update is accepted only when both the sender and chat match their allowlists. Private-chat IDs normally match the user ID; group IDs are usually negative numbers.

### Settings

| Setting | Purpose |
| --- | --- |
| `telegram.token` | Telegram bot token. Prefer an environment reference. |
| `telegram.allowed_users`, `telegram.allowed_chats` | Required numeric ID allowlists. Values may be literal integers or comma-separated environment values. |
| `telegram.progress_mode` | Live task progress: `off`, `summary`, or `verbose`. The default reads `OMP_TELEGRAM_PROGRESS_MODE`; empty or invalid values fall back to `summary`. See [Progress UI](#progress-ui). |
| `telegram.progress_recent_tools` | Maximum completed tools retained in verbose Recent history. Default: `5`; range: `1..100`. Display may show fewer to fit Telegram's message limit. |
| `telegram.final_actions` | Optional ordered completion shortcuts `{ label, command }`. Omitted or `[]` disables them. At most six buttons; labels are nonempty, single-line and at most 64 UTF-16 units. Commands are one slash invocation with optional literal fixed arguments and no controls or line breaks. See [Completion shortcuts](#completion-shortcuts). |
| `telegram.status_bar.progress`, `.intermediate`, `.final` | Ordered field arrays for Progress, intermediate assistant messages, and final results. `[]` disables that header. Omitted arrays default to `["status", "thinking", "model", "fast", "context", "speed"]`. See [Status bars](#status-bars). |
| `telegram.status_bar.git_command` | Optional literal argv for the final `git` field. Omitted or `[]` uses the built-in safe Git probe. Custom commands run without a shell in the current session workspace; users own their safety and side effects. |
| `telegram.status_bar.git_timeout` | Positive literal Go duration for both built-in and custom Git sampling, default `"450ms"`. Increase to e.g. `"2s"` for large repositories. Empty, zero, negative, invalid, overflowing or non-string values are rejected; environment references are not expanded and the timeout cannot be disabled. |
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

Selections from `/model`, `/thinking` and `/args` update the original menu with the result instead of sending a duplicate completion message. If there is no menu or it cannot be updated, the result is sent once as a separate notification.

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

Progress appears after about three seconds and includes **Stop | Follow-up** in one row. **Stop** cancels the current task; **Follow-up** opens a reply prompt without pausing or steering it. With the `status` field selected, its **⚙️** marker shares the first line with available status fields. `/stop` also clears waiting tasks. Progress updates and cleanup are best-effort and do not delay formal replies. `progress_mode = "off"` creates neither button.

Steer input replaces the current Progress preview without restarting OMP. After an assistant message completes, its older preview is frozen and removed only after all corresponding formal reply parts are successfully delivered. Failed or uncertain delivery preserves that visible copy; a later Steer cannot bypass this protection. A new preview waits for preceding formal replies to leave the delivery queue and appears below the latest input/reply. Cleanup retries include superseded previews of running tasks; formal replies are never cleanup targets.

A native local command confirmed not to invoke the model retires its status-only Progress after its completion transaction commits, even when it creates no formal assistant reply. Late preview acknowledgments and temporary deletion failures follow the same cleanup policy.

Completed assistant messages are sent immediately, even with progress off. With the `status` field selected, intermediate headers show **💬** and confirmed task success changes only the latest formal header to **✅**; cancellation, failure, uncertainty and ongoing research rounds do not gain a success marker. Only the first formal message quotes the original request. Bodies stay unchanged for Telegram Reply, with no body resend or separate completion message.

### Follow-up input

Click **Follow-up**, then reply to the bot's ForceReply prompt with nonempty text. Only the clicking user's reply to that particular prompt in the same chat/topic is accepted, as a separate queued task just like `/followup <message>`. Opening the prompt does not pause the running task. After acceptance, the bot confirms that the follow-up was queued; a full queue or empty/nontext reply does not consume the request or report success, so you can retry while it is valid.

Requests expire after ten minutes. Clicking again replaces your previous request in that conversation. Natural task completion keeps the issued request valid if the session/binding is unchanged; Stop, Close, session replacement and service restart invalidate unfinished requests. Replies to recognized stale, unauthorized or uncertain-delivery prompts are rejected, including `/stop`, `/close` and other commands. Only valid requests allow recognized Bridge commands to use their ordinary control path. A well-formed marker from this bot is rejected even if its database record is missing. Ordinary text not replying to a recognized prompt keeps its existing prompt/steer behavior. Telegram clients control how ForceReply is displayed; the bridge does not provide an inline text-entry widget.

Marker fallback requires a complete `[followup:token]` line at the very end of a nonempty prompt body. Inline fragments and markers followed by more text do not match. Stored prompt IDs remain authoritative even if the quoted marker is omitted.

### Completion shortcuts

To opt in, place this setting in the existing `[telegram]` table, before `[telegram.status_bar]`:

```toml
final_actions = [
  { label = "Compact", command = "/compact" },
  { label = "New", command = "/new" },
  { label = "Shake", command = "/shake" },
]
```

Omit the setting or use `final_actions = []` to disable it (the default). Order is preserved; labels are user-defined and are not translated. Labels must be nonempty and single-line, with at most 64 UTF-16 units (an emoji outside the BMP uses two). Up to six entries are allowed. Each command must start with a slash and contain one command name, optionally followed by fixed arguments; no newlines or control characters are allowed. Labels and commands stay literal: `$VAR`, `${VAR}`, shell syntax and argument text are never expanded or executed by a shell. Arguments retain the existing command's parsing rules, not a second shortcut language. Do not put secrets in visible button labels or command arguments.

Rows contain at most three buttons and use a 24-UTF-16-unit combined-label budget to move longer labels onto additional rows. A single long label remains intact on its own row; Telegram clients determine actual button widths.

Buttons appear only on the last real assistant body part after the root task successfully completes, including when status headers are hidden. They do not appear on intermediate replies, error/truncation notices, stopped/uncertain results or native command output. Updating buttons does not resend the answer. Each button is one-shot after command admission, not after the first click: deterministic local rejection such as busy state, invalid arguments or unavailable commands leaves it retryable with a new click. A reservation prevents overlapping clicks while eligibility or an execution confirmation is pending; cancelling or expiring that confirmation releases it. Successfully opening a command's menu counts as executing that menu command. Accepted queue input, target RPC submission and local execution consume the button; submitted or uncertain operations are never replayed. A reservation interrupted by restart is conservatively consumed. Another unused button or a later completed reply can still be used. Changed sessions/bindings or removed/changed configuration invalidate old actions.

Clicks use the same chat/topic, the real clicking user's permissions, original command arguments, busy-state restrictions and confirmations as typing the command. Unknown or unavailable commands give an error, never become a model prompt or silently run a different action. Native shortcuts require an executable command advertised by the **current session's** native command catalog and remain subject to source/lifecycle restrictions; configuration loading does not prove availability. The native OMP catalog used for this implementation advertised builtin `/shake` ("Shake heavy content out of the conversation context"), so the example maps **Shake explicitly to `/shake`**. Availability can differ by runtime/session: an unavailable `/shake` rejects, and is never automatically mapped to Queue or another operation.

### Status bars

Configure the three header types independently. Fields appear in array order; unknown or duplicate names are rejected. `[]` removes the entire header, including its icon and separator. Disabling the Progress header does not hide its tool body, text preview or **Stop | Follow-up** buttons; use `progress_mode = "off"` to disable Progress itself.

```toml
[telegram.status_bar]
progress = ["status", "model", "context"]
intermediate = []
final = ["status", "model", "thinking", "fast", "context", "speed", "quota", "git"]
```

| Field | Display |
| --- | --- |
| `status` | Processing, assistant or confirmed-completion icon. |
| `model` | Literal model ID, without provider. |
| `thinking` | Configured thinking selector symbol. |
| `fast` | Confirmed fast-mode activity/setting. |
| `context` | Confirmed usage and window, e.g. `46% (500k)`. |
| `speed` | Native live speed or confirmed reply average, e.g. `3.8 t/s`. |
| `quota` | Native 5h/7d used percentages from the same-account cache. |
| `git` | Final results only: current branch and optional dirty `*`, or custom command stdout. |

Unavailable or unread values are omitted, including failed reads: no `ctx ?`, `quota ?` or `n/a` placeholders. Already confirmed values may appear immediately. Refreshes do not backfill older intermediate messages. Fields that are not selected do not enable display-only sampling; task-safety and command queries are unchanged. Detailed `/status` is unchanged.

`git` is sampled at completion in the session's current workspace, not at task startup. With `git_command` omitted or empty, the built-in read-only probe shows `main` when clean and `main*` for staged, unstaged or nonignored untracked changes; ahead/behind alone is not dirty. Detached HEAD shows `HEAD`. Outside a repository, on failure or when the bounded probe cannot finish, the field is omitted, never presented as known clean. Ongoing research rounds do not sample Git; root settlement does.

The built-in probe checks tracked-file attributes and initialized submodules before reading status. If content filters would make a safe read-only result unavailable, it omits the Git field instead of executing them; unused global filter definitions do not hide ordinary repositories. Trace2 output is explicitly disabled even when global/system Git configuration enables it. The bounded probe is not an atomic snapshot of concurrent Git configuration or attribute changes.

To override the `git` field, configure a nonempty `git_command` argv:

```toml
[telegram.status_bar]
final = ["status", "model", "context", "speed", "git"]

# Empty uses the built-in safe Git status implementation.
git_command = []
# Shared by built-in and custom Git sampling; increase to "2s" for large repositories.
git_timeout = "450ms"

# Custom commands run with the session workspace as the working directory.
# Users are responsible for the safety and side effects of custom commands.
# git_command = ["git", "log", "-1", "--format=%h"] # a1b2c3d
# git_command = ["/usr/local/bin/my-git-status"]    # dev@a1b2c3d*
```

Custom argv entries are literal: no shell, environment-reference expansion or extra Git arguments. The command uses the existing `omp.environment` child policy, without the built-in probe's Git-specific environment rewriting or safety preflight. It runs only when `final` selects `git`. Its stdout is displayed literally, not parsed as Git output: trailing CR/LF is removed, other spaces are preserved, and a single valid UTF-8 line is clipped to 64 bytes at a character boundary before Telegram escaping. Commands use `git_timeout` and retain the 64 KiB stdout cap; failures, timeouts, empty output, embedded line breaks/control characters or invalid UTF-8 omit the field, with no built-in fallback. Stderr is discarded. Research settlement may re-probe; do not rely on exactly-once invocation. Later quota edits retain the captured output rather than rerunning the command.

`git_timeout` applies to the entire built-in safety preflight/status probe or to the custom command, without retries. Its default is `"450ms"`; large repositories can use e.g. `"2s"`. It must be a positive literal Go duration string: environment references are not expanded, and empty, zero, negative, invalid, overflowing or non-string values are rejected. The timeout cannot be disabled. Increasing it does not relax the built-in safety checks, the 64 KiB output cap or the 64-byte label limit; large repositories can still omit the field if they exceed the output cap.

Git's budget is separate from native status/account RPC deadlines: each ordinary final-sampling RPC remains bounded to 1 second, each research-settlement RPC to 5 seconds. Ordinary sampling shares cancellation while giving Git its own configured budget. Research settlement keeps operation cancellation and ownership/session fences, but no longer has a generic 30-second operation cap: its finite RPCs and Git probe each enforce their own deadlines.

Migration: the old `telegram.provider_usage` key is ignored and produces an ERROR log, but no longer prevents startup. Remove it and add `"quota"` only to the arrays where it should appear; omit it everywhere to disable quota reads. Quota and Git are not selected by default.

When only intermediate/Progress quota is selected, an idle runtime can still identify the account and warm the cache after a short task; completion never waits for it. Quota selected only in disabled Progress does not enable reads. Oversized headers omit whole fields rather than cutting a literal value or its dirty marker.

Replies support Markdown and split long text automatically. Large tables may use a plain-text layout; formatting unsupported by Telegram falls back to readable text.

Selecting `quota` enables native Claude (`anthropic`) or OpenAI Codex (`openai-codex`) account quota in the selected headers, independently of progress mode. The field appears only after a current-scope account read confirms a usable same-account cache value, for example `5h30% 7d65%`. Only 5h/7d used percentages are shown, without a reading timestamp; account identities, emails, credential IDs and raw reports are not sent to Telegram or written to logs or the database.

Cache visibility does not trigger or wait for refresh. Refresh uses the existing native builtin `/usage` only while idle, with at least five minutes between attempts for the same account scope and a three-second deadline; queued prompts and controls take priority. Busy or long-running tasks show the cache when available, otherwise omit the field. Failed, skipped or empty reads do not clear a last-good value. A successful refresh may update the latest matching final header only when its array selects `quota`, never its body or captured Git result. No direct provider API request is made.

Final answers supplied only by `agent_end.messages` also keep an editable header. Later quota refreshes update that final answer, not an earlier intermediate reply.

The cache lives only in the Bridge process and is not persisted across daemon restarts. It can be shared across messages and native sessions only for the same startup argv namespace and the same uniquely active OAuth credential reported by `get_logout_accounts`. Account scope must match before and after the quota query. Ambiguous or unknown scope never displays another account's cached value; the stored value is retained. These separate RPC reads are not an atomic account-and-quota snapshot and cannot attribute each rotating request to its actual billed account.

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

`storage.database_retention_days` controls retention of terminal bridge messages, eligible attachment caches and expired Follow-up request metadata. Default: `90` days. Set it to `0` to disable automatic retention cleanup. Follow-up age uses its expiration time; unread expired prompts are eligible too, while correlations with pending/submitted inbox work are retained until settled. Workspace files, OMP session data, and saved conversation bindings are not removed. Retention does not delete delivered Telegram replies.

Before upgrading, stop the service and back up the data directory, workspaces, and OMP's own session storage. The bridge database alone is not a complete backup of OMP conversations.

Authorize trusted users only. The service runs with the filesystem permissions and environment of its OS user, and group members may see prompts and replies even when they cannot control the bot. Keep the bot token private.

## Logging

Logs support `text` and `json`, selected by `logging.format`. `logging.level` sets the global threshold; `[logging.component_levels]` overrides it for `daemon`, `bridge`, `rpc`, `telegram`, `store`, or `media`. See the complete [configuration example](#configuration).

Logs are written to stderr; use your process manager for storage and rotation. Prompts, model replies, credentials, tool payloads, and workspace/session paths are not logged. Chat and thread IDs may still identify conversations, so restrict log access.

`rpc_queue_overflow` is an ERROR visible at the default info level. `reason=event_slots|byte_budget` and `stage=event_enqueue|physical_read|chunk_reassembly` identify the exhausted resource and detection point. Compare `queued_events/queue_capacity`, `rejected_used_bytes/rejected_bytes/buffer_limit_bytes`, and `forward_wait_ms`; `buffered_bytes` is sampled after rejected-frame cleanup, not the occupancy at rejection. Correlate `client_id` with `consumer_phase/consumer_phase_ms` and nearby `rpc_consumer_slow` WARN records. The latter report actor operations taking at least one second, with a fixed `point`, duration and task identity. Normal fast operations do not produce these warnings; RPC frames and tool content are never included. These snapshots help locate pressure or blocking but do not by themselves prove its root cause.

## Roadmap

- [ ] **Undo/rewind and redo.** Requires reliable input-to-session tracking and remote rewind support from OMP. Track [#12991](https://github.com/can1357/oh-my-pi/issues/12991), [PR #10026](https://github.com/can1357/oh-my-pi/pull/10026), and [PR #11663](https://github.com/can1357/oh-my-pi/pull/11663).
- [ ] **Reuse the OMP process after Stop.** Requires reliable upstream cancellation. For now, Stop retires the process while preserving its session.
- [ ] **Cancel individual native queued prompts.** Requires reliable upstream cancellation results. `/queue` currently cancels only bridge-pending tasks.
- [ ] **Sub2API account quota.** Requires the deployed fix for [#8473](https://github.com/Wei-Shaw/sub2api/issues/8473) ([PR #8476](https://github.com/Wei-Shaw/sub2api/pull/8476)), quota-read permission, and reliable attribution to the account serving each reply.
- [ ] **Structured native account-and-quota snapshots.** Wait for an upstream interface with a reliable opaque account scope, per-account report timestamps and refresh without a prompt, replacing the identity displayed in native reports and the before/after scope checks. Existing RPC reads are not atomic and cannot identify the account actually billed for each rotating request.

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
