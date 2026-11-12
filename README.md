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
- The latest stable OMP release, plus its runtime, such as Bun. Only the latest stable release is a compatibility target; RPC protocol v2 support alone does not establish compatibility. Configure models and authentication for the same user that will run this service.
- Git is needed only for OMP's native autoresearch workflows.
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

For arm64, use `omp-telegram-linux-arm64.txz`. Archives contain the binary and LICENSE; omp itself is installed separately. The optional migration Extension is embedded in the binary. Each release includes `SHA256SUMS`.

Development builds are published as the [Development build](https://github.com/fcying/omp-telegram/releases/tag/dev-latest). They are prereleases and may be unstable.

### Build from source

Requires Go 1.27.4 or later (see [go.mod](go.mod)) and [just](https://github.com/casey/just):

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

Copy [config.toml](config.toml) beside the binary for a complete annotated example, or start with this minimal configuration. Other settings use the defaults in the [settings reference](#settings).

```toml
[telegram]
token = "${OMP_TELEGRAM_BOT_TOKEN}"
allowed_users = ["${OMP_TELEGRAM_ALLOWED_USERS}"]
allowed_chats = ["${OMP_TELEGRAM_ALLOWED_CHATS}"]
progress_mode = "${OMP_TELEGRAM_PROGRESS_MODE}"

# Keep the bot token out of OMP's inherited environment.
[omp.environment]
mode = "denylist"
deny = ["OMP_TELEGRAM_BOT_TOKEN"]
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
| `telegram.status_bar.progress`, `.intermediate`, `.final` | Ordered field arrays for Progress, intermediate assistant messages, and final results. Nested arrays group fields with spaces; outer items use ` \| `. `[]` disables that header. Omitted `intermediate` defaults to `[]`; `progress` and `final` default to `["status", ["thinking", "model", "fast"], "context", "speed"]`. See [Status bars](#status-bars). |
| `telegram.status_bar.git_command` | Optional literal argv for the final `git` field. Omitted or `[]` uses the built-in safe Git probe. Custom commands run without a shell in the current session workspace; users own their safety and side effects. |
| `telegram.status_bar.git_timeout` | Positive literal Go duration for both built-in and custom Git sampling, default `"450ms"`. Increase to e.g. `"2s"` for large repositories. Empty, zero, negative, invalid, overflowing or non-string values are rejected; environment references are not expanded and the timeout cannot be disabled. |
| `omp.binary` | OMP executable name found through `PATH`, or an absolute path. Default: `"omp"`. It is not a shell command. |
| `omp.args` | Ordered OMP startup presets. Defaults to one optional `OMP_TELEGRAM_ARGS` preset. A legacy scalar string is accepted as one preset. |
| `omp.args_labels` | Optional Telegram picker labels, one per preset; invalid labels fall back to `Preset N`. Labels are visible in the conversation, so never put credentials in them. |
| `omp.migration_extension` | Load the bundled migration Extension on every OMP startup, including resume, for all argument presets. Default: `false`. No separate Extension installation is required. |
| `omp.auto_session_name` | Opt in to one native `/rename` attempt for newly created sessions. Default: `false`; may incur extra model calls and cost. See below. |
| `[sub2api]` | Optional bridge-only upstream quota: `base_url`, `api_key`, and exact OMP `providers`. All empty disables; partial configuration fails. See [sub2api upstream quota](#sub2api-upstream-quota). |
| `sub2api.extension` | Enable the bundled sub2api quota Extension for all argument presets. Default: `false`; required when the quota source is enabled. |
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

With `omp.auto_session_name = true`, only newly created sessions qualify: the first successful ordinary task triggers a one-shot native `/rename` after its assistant body is committed to the outbox. Existing names are not overwritten, Telegram topic titles are unchanged, and historical sessions are not backfilled (including resume, fork, and migration). Naming is best-effort, may incur extra model fees, and does not guarantee a title on failure; use `/name <title>` or native `/rename` manually instead.

If automatic naming state cannot be saved, the bot reports that naming was skipped; the task result stays saved and the session remains usable. Naming is checked again only after another successful ordinary task.

If `/name` cannot confirm that a preceding automatic rename has passed native input handling, it reports an unconfirmed change without writing the requested name. If this persists, use `/close` followed by `/resume` to reconnect before retrying `/name`.

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
| `/queue` | View running and queued work. Cancel buttons remove individual bridge-pending tasks, not the active task or native queued prompts. |
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
| `/title <title>` | Ask Telegram to rename the current topic. Does not rename the OMP session. |
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

On a topic's first session, the bridge attempts to use the workspace name as its title, adding `[thread ID]` for a known duplicate in the same chat. A successful `/title` prevents this automatic rename, including after worker recreation or restart. For a title set before this protection was introduced, repeat `/title` once after upgrading. Telegram cannot provide a complete topic list, so unobserved or manually renamed titles may cause missed collisions or unnecessary suffixes. Other topics are not renamed.

Use `/title Trading research` inside the topic to rename. No OMP session is required, and a running task is not interrupted. The reply reports Bot API acceptance or failure, not whether clients display the new title; synchronization is not guaranteed ([upstream issue](https://github.com/tdlib/telegram-bot-api/issues/915)). If another rename is pending, wait and retry. In supergroups, the bot needs administrator status and `can_manage_topics`, unless it created a non-General topic. General always requires these permissions.

A successful model or thinking change can still show `Thinking: n/a` when the configured level is unavailable. This does not undo the change; if the process must close, the reply says that the session was kept.

Selections from `/model`, `/thinking` and `/args` update the original menu with the result instead of sending a duplicate completion message. If there is no menu or it cannot be updated, the result is sent once as a separate notification.

Session (`/resume`, `/export`), binding, model-role and argument-preset lists show names and identifying details in the message body. Buttons use matching numbers and short actions such as **Pin 2** or **Delete 2**. Long entries use content-based pages, with the same number on continuation pages. Queue task previews also appear in the body, with numbered Cancel actions. Paging does not select, delete, or cancel an item; destructive actions retain their confirmation steps. `/args` shows configured labels or preset numbers, never raw arguments.

Ask, native selection, model-role and argument-preset menus arrange numbered choices in rows of up to five buttons. Controls stay on separate rows; session, binding and queue actions keep their per-entry rows.
Ask questions and completed native-selection receipts use compact section spacing while preserving line breaks and blank lines inside the original question and answers. Adjacent options do not receive extra blank lines.

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

Finish pending dialogs and replies before exporting the current session. New native dialogs are unavailable during that export and are not reopened afterward; request a new dialog explicitly if still needed. Exporting a different saved session does not block the current dialog.

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

### Native Ask questions

The bridge uses OMP's `rpc-ui` mode without changing its enabled tools or approval policy. If native `ask` is enabled in OMP, ask the agent to use it, for example: "Use Ask to let me choose between Programming, Investing, and Languages."

Native `ask` takes an HTML form as `{html}` since OMP 18.18.3, not the old `questions[]` tool schema. In RPC without `formDialog` (including OMP 18.18.6), OMP presents each field through generic `select`/`input`/`editor` requests; checkbox fields use repeated selections. The bridge handles those requests, not the HTML, and does not provide a separate form renderer. Use the displayed options and reply to the exact question or dedicated input prompt shown by the bridge.

For extension Ask and all native selection menus, including Review, the message body shows each option's full label, description and preview with their original line breaks and spacing. Selection buttons contain only global option numbers; extension multi-choice buttons also show ☐/☑. Navigation, confirmation, cancellation and custom-answer controls keep short action labels. Pages contain at most ten options and may contain fewer when content is long. An oversized option continues on later pages under the same number; paging never submits an answer. After a native selection, the original main message retains the question, complete options and local choice when the full receipt fits in one message. If it does not fit, the bounded receipt marks omitted content with `...`; the RPC still receives the original value. A subsequent native select may reuse the same menu with earlier action summaries. Checkbox fallback records individual selections only, not a final checked set, and does not treat labels such as "Done" specially. Display formatting and pagination never change the original values sent to OMP.

Text-field replies preserve whitespace and newlines. **Use default** accepts the native editor's prefilled value; **Submit empty** sends an empty string without cancelling. In private chats, including bot topics, text fields use one question with buttons and automatic Reply. Reply directly to that question; submission or cancellation removes its buttons without appending **Submitted** or sending a separate input/closing message. In groups, the main question still retains **Submitted** and the actual value; an empty submission has a distinct zero-character status. OMP validates required fields, numbers and patterns and may ask for the same field again; UI status is not validation success. Placeholders are hints, not defaults. Display clipping does not change RPC answers or defaults. UI updates are best-effort. The native RPC fallback does not provide whole-form layouts, images or initial checkbox-selection markers.

Extensions independently retain questions-based `ui.askDialog` and the `set_ask_dialog` opt-in, delivered as RPC `method:"ask"`. For these combined extension dialogs:

- Each question has its own independent main message. Confirmed local choices remain with that question instead of being replaced by the next one. Single-choice buttons advance immediately; multi-choice buttons toggle selections, then **Confirm selection** submits them. An explicitly confirmed empty multi-selection is allowed. All answers are sent together exactly once after the final question.
- **Custom answer** opens a separate reply prompt. Reply to that exact message with nonempty text; these combined-dialog custom answers are trimmed. Multi-choice answers can include both selected options and custom text.
- Cancel, Stop and native cancellation keep the current question and all displayable options in its original message with a cancelled status, not an answer. Earlier confirmed choices remain unchanged; unconfirmed selections are not submitted.

For both native forms and extension dialogs:

- Ordinary guidance sent while a task waits for Ask is buffered, then forwarded to that same running task after the dialog/tool finishes. It does not answer or cancel Ask, and is not a separate followup. Buffered guidance shares the queue limit; if the original task ends, is stopped, or its session/runtime is replaced before dispatch, it is cancelled without replay. Resend it explicitly if needed. Controls such as `/stop` are not held behind it.
- **Back to options**, when offered by a combined dialog, cancels only text entry. **Cancel** cancels the native interaction; **Stop** stops the task. OMP owns timeout/default-answer behavior. Old buttons and reply prompts cannot answer a replacement question or session.
- Questions, input prompts and your replies are retained. Private native text fields finish by clearing the question's own buttons. Group text fields and combined extension dialogs keep their separate input prompt and send a short completion receipt to dismiss automatic Reply. Old completion jobs do not dismiss newer input; delivery is best-effort.
- Bridge commands such as `/stop` and the native `/autoresearch off` control keep their normal effects even when replying to an expired or still-delivering Ask prompt. Commands addressed to another bot are ignored, never used as answers.
- Only the task's originating user can answer. One native interaction (Ask, selection, confirmation, or text input) is active per conversation; additional concurrent requests are cancelled without replacing its menu. After answering, the next dialog may briefly wait for the previous response delivery to settle. Duplicate request IDs never reopen dialogs or replay answers. Answers continue the existing task, never create a new prompt, and are not replayed after failure or restart.

Combined extension dialogs are enabled automatically when OMP supports the opt-in; this does not enable HTML `formDialog`. An explicit unsupported-command response keeps `rpc-ui` and uses generic selection/plain-text entry instead; other negotiation failures abort startup. Sensitive inputs marked secret and arbitrary terminal editors are not supported.

Dialogs that block OMP startup before connection are not supported: startup times out and fails, with no actionable Telegram question or buttons. If an unanswered confirmation or other RPC dialog is detected at timeout, Telegram reports the detection, not a proven cause. Configure the extension for noninteractive startup or move its question to a user-invoked command; follow any `/close` instruction before retrying. The bridge never automatically answers startup dialogs. See [native dialog implementation](doc/architecture.md#native-dialogs-and-session-operations).

### Troubleshooting and limitations

| Symptom | Check |
| --- | --- |
| Bot does not respond | Both allowlists, group topics, group privacy settings, and whether another poller or webhook uses the bot. |
| `omp.binary executable not found` | The configured executable and the service process's `PATH`, including omp and its runtime. It may differ from your interactive shell. |
| `/resume` shows no sessions | The selected directory and whether OMP has saved history yet. A new session may have no resumable file. |
| `/compact` fails | Short sessions may have nothing to compact. Check OMP's model configuration if local compaction also fails. |
| Unsupported database schema | Back up the data and use a supported database or fresh data directory; do not change a version number by hand. |

Undo/rewind and redo, voice and transcription, automatic topic creation, and arbitrary terminal/editor interfaces are not supported. Native Ask, selection, confirmation, and plain-text input dialogs are supported as described above, but not every interactive tool approval is available remotely. Automatic approval is never enabled by the bridge.

Very large output or many replies may be truncated with an explicit notice. OMP's native session history is not truncated.

## Progress UI

`telegram.progress_mode` controls live task progress:

- `off` disables progress messages and typing actions.
- `summary` shows active tools, task state, and the current assistant text preview.
- `verbose` additionally shows recent observable tool activity. Tool details are unredacted and may contain credentials or private content visible to everyone who can read the conversation.

Progress appears after about three seconds and includes **Stop | Follow-up** in one row. **Stop** cancels the current task; **Follow-up** opens a reply prompt without pausing or steering it. With the `status` field selected, its **⚙️** marker shares the first line with available status fields. `/stop` also clears waiting tasks. Progress updates and cleanup are best-effort and do not delay formal replies. `progress_mode = "off"` creates neither button.

Steer input updates the current preview without restarting OMP. Previews are normally removed after formal replies arrive; failed or uncertain delivery can leave a preview visible. Formal replies are not deleted by progress cleanup.

Completed assistant messages are sent immediately, even with progress off. With `status` selected, intermediate headers show **💬** and confirmed task success marks the latest formal reply **✅**. Cancellation, failure, uncertainty and ongoing research rounds do not gain a success marker. Only the first formal message quotes the original request; completion updates do not resend the answer.

### Follow-up input

Click **Follow-up**, then reply to the bot's ForceReply prompt with nonempty text, a photo or a file; attachments may include a caption and use the normal attachment limits. An album is queued as one task. Only the clicking user's reply to that particular prompt in the same chat/topic is accepted as a separate queued task. Opening the prompt does not pause the running task. After acceptance, the bot confirms that the follow-up was queued. A full queue or empty/unsupported reply does not consume a new request, so you can retry while it is valid. If attachment download or preparation fails after acceptance, the task fails without automatic replay; open a new Follow-up request to retry.

In private chats, the prompt includes **Cancel**, which revokes only that input request, never an already queued task. Acceptance replaces the waiting instructions with a short queued status and removes its buttons; clicking **Cancel** shows only the cancelled status. Neither sends a separate confirmation or closing message. Groups retain selective ForceReply and a separate queued/completion receipt. All prompts and replies remain, preserving quoted references instead of leaving `Deleted message`. UI updates are best-effort and do not wait for queued task execution.

Requests expire after ten minutes. Clicking again replaces your previous request in that conversation. Natural completion keeps it valid if the session is unchanged; Stop, Close, session replacement and service restart invalidate unfinished requests. Replies to stale, unauthorized or uncertain-delivery prompts are rejected, including commands: send a command as a new message instead. A valid prompt accepts recognized Bridge commands with their normal effects. Other ordinary text keeps its usual prompt/steer behavior. ForceReply presentation depends on the Telegram client; there is no inline text-entry widget.

### Completion shortcuts

To opt in, place this setting in the existing `[telegram]` table, before `[telegram.status_bar]`:

```toml
final_actions = [
  { label = "Compact", command = "/compact" },
  { label = "New", command = "/new" },
  { label = "Shake", command = "/shake" },
]
```

Omit the setting or use `final_actions = []` to disable it (the default). Order is preserved and labels are not translated. Up to six entries are allowed. Labels must be nonempty, single-line and at most 64 UTF-16 units (an emoji outside the BMP uses two). Commands are one slash invocation with optional fixed arguments, without newlines or control characters. Labels and commands are literal: no environment expansion or shell execution. Do not put secrets in visible labels or arguments. Buttons wrap into rows of at most three; long labels may occupy a row alone.

Buttons appear on the last assistant reply only after successful task completion, even with status headers hidden. They do not appear on intermediate replies, error/truncation notices, stopped/uncertain results or native command output. Each button can execute once; busy state, invalid arguments or an unavailable command allow another click. Pending confirmations prevent overlapping clicks; cancelling or expiring a confirmation allows retry. Opening a command's menu counts as execution. Submitted or uncertain actions are not replayed, including actions interrupted by restart. Session changes and removed or changed shortcut configuration invalidate old buttons; other unused buttons remain available.

Clicks use the clicking user's permissions and the same chat/topic, arguments, busy-state restrictions and confirmations as typing the command. Unknown or unavailable commands report an error, never become a model prompt or a different action. Native commands must be available in the current session and obey the [native command limitations](#native-omp-commands). For example, **Shake** invokes `/shake` only when OMP supports it; there is no fallback action.

### sub2api upstream quota

This is an opt-in source for the status bar's `quota` field, not a model provider configuration. Keep OMP's existing provider URL, model/context settings and credentials unchanged. The bridge does not automatically change models, providers or global OMP configuration. Merge the following tables into the bridge's `config.toml`:

```toml
[sub2api]
extension = true
base_url = "https://sub2api.example.com"
# usage.read (required, global): Read all-site logs to identify the serving account.
# openai.quota.read (required, resource): Scope to the target OpenAI OAuth groups.
# accounts.identity.read (optional, resource): Show plan names instead of account IDs.
api_key = "<management-key>"
providers = ["my-openai-gateway"]

[telegram.status_bar]
final = ["status", "model", "context", "speed", "quota"]
```

`base_url`, `api_key` and each `providers` element use the existing one-pass environment expansion. All three empty disables this source; partial settings, invalid HTTP(S) URLs, duplicate/invalid provider IDs, or an enabled source without explicit `sub2api.extension = true` fail startup with redacted errors. URLs may have a path prefix, but not credentials, query, fragment or dot-path segments. Use HTTPS for remote administration. Provider matching is exact and case-sensitive: use the actual OMP provider ID, not a model name, URL or guessed alias. Add `quota` independently to `progress`, `intermediate` and/or `final`; defaults remain unchanged.

The management key needs global `usage.read` and resource `openai.quota.read` scoped to the target OpenAI OAuth groups. Global `usage.read` exposes all-site logs, not only the selected groups. Optional resource `accounts.identity.read` adds plan labels; otherwise confirmed account IDs label the quota.

Keep the management key private, use HTTPS for remote administration, and do not reuse model credentials. The key is used by the bridge, not added to OMP arguments. If using an environment reference for it, explicitly exclude that variable from OMP's inherited environment with [the child environment policy](#settings).

Configuration validation rejects enabled sub2api with `--no-ui` in any OMP argument preset; remove that option to enable quota. RPC does not require an interactive terminal. `--no-extensions` remains compatible with this explicitly enabled Extension.

HTTP and SSE responses support request attribution through the server's `x-client-request-id` response header. WebSocket is not supported: a `101` handshake does not establish the serving account for each later turn, and the bridge does not infer per-turn usage from a handshake ID. The bridge does not change OMP's transport configuration; WebSocket requests have no attributed sub2api quota.

Once the task has finally completed and the worker is idle, the bridge resolves all of that task's request IDs to actual upstream OpenAI OAuth accounts and deduplicates those accounts. Requests from multiple configured provider IDs in the same task contribute to this shared account set. The field shows their recognized 5h/7d **used** percentages and available reset countdowns, using the [status-bar format](#status-bars) below; multiple accounts can appear separately. Missing or ambiguous attribution, unsupported OMP hooks/headers and unavailable quota omit the affected values. The bridge never borrows a previous task's identity, guesses an account from a model, or substitutes gateway credential quota.

Usage logs may appear after task completion. While the task scope remains valid and the worker is idle, bounded attribution retries can backfill the latest corresponding final header without delaying completion, changing the answer or replaying the task. Research retains attribution from the round that produced the actual body, not a later empty round. Queued work, a new scope or a retired runtime take priority; historical headers are not guaranteed to be backfilled.

Native and sub2api quota share the same idle-only refresh, cache and rendering flow; only account attribution and quota acquisition differ. Refresh attempts for the same account are gated for five minutes. Refreshing, failed reads and empty results retain the last successful sample without a display TTL, but a new task must confirm its own account before reusing cached quota. Keys and raw responses are not logged or sent to Telegram; detailed `/status` is unchanged. See [architecture](doc/architecture.md#sub2api-quota) for implementation details.

### Status bars

Configure the three header types independently. Fields appear in array order. Outer items use ` | `; fields within a nested array use spaces. Flat arrays remain supported. Unavailable fields and empty groups leave no separators; unknown names and duplicates, including across groups, are rejected. `[]` removes the entire header, including its icon and separator. Disabling the Progress header does not hide its tool body, text preview or **Stop | Follow-up** buttons; use `progress_mode = "off"` to disable Progress itself.

```toml
[telegram.status_bar]
progress = ["status", "model", "context"]
intermediate = []
final = ["status", ["thinking", "model", "fast"], ["context", "speed"], "quota", "git"]
```

| Field | Display |
| --- | --- |
| `status` | Processing, assistant or confirmed-completion icon. |
| `model` | Literal model ID, without provider. |
| `thinking` | Configured thinking selector symbol. |
| `fast` | Confirmed fast-mode activity/setting. |
| `context` | Confirmed used percentage of the context window and its capacity, e.g. `46% (500k)`. |
| `speed` | Native live speed or confirmed reply average, e.g. `3.8 t/s`. |
| `quota` | Used percentage of native or actual upstream account quota, with available reset countdowns. |
| `git` | Final results only: current branch and optional dirty `*`, or custom command stdout. |

Unavailable or unread values are omitted: no `ctx ?`, `quota ?` or `n/a` placeholders. Quota refresh failures retain any confirmed last-good sample. Confirmed values may appear immediately; refreshes do not backfill older intermediate replies. Oversized headers omit whole fields rather than cutting values. Detailed `/status` is unchanged.

Both `context` and `quota` show **percentage used**, not remaining: higher values mean closer to the limit. `context` measures the occupied context window; `quota` measures account usage in each rate-limit window. With both quota windows, `20%(2h) 60%(5d12h)` means 20% used of the 5-hour quota, resetting in 2 hours, then 60% used of the 7-day quota, resetting in 5 days 12 hours. A single window keeps its label, e.g. `7d 60%(5d12h)`. `~` marks approximate reset times, e.g. `(~6d)`; an exact countdown below one hour shows `(<1h)`. Missing or elapsed reset times omit only the countdown, without assuming quota has recovered. Countdowns update when a header is rendered, not continuously in old messages.

`git` shows the current workspace's branch at task completion: `main` when clean, `main*` for staged, unstaged or nonignored untracked changes, and `HEAD` for detached HEAD. Ahead/behind alone is not dirty. It is omitted outside a repository or when a safe read fails or times out, including repositories whose content filters prevent a safe read. Ongoing research rounds do not show a new Git sample until research finishes.

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

Custom commands run only when `final` selects `git`, using the current workspace and `omp.environment` policy. Arguments are literal: no shell or environment expansion. Unlike the built-in probe, custom commands have no Git-specific safety protections; use a safe, read-only command and do not rely on exactly-once invocation. Stdout must be a nonempty valid UTF-8 single line; trailing line endings are removed and the label is clipped to 64 bytes. Failure, timeout, invalid output or output above 64 KiB omits the field without falling back to the built-in probe. Later quota updates do not rerun it.

`git_timeout` defaults to `"450ms"`; increase it to e.g. `"2s"` for large repositories. It must be a positive literal Go duration string, without environment expansion, and cannot be disabled. Increasing it does not relax safety or output limits, so the field may still be omitted. See [architecture](doc/architecture.md#git-status-sampling) for Git sampling details.

Migration: the old `telegram.provider_usage` key is ignored and produces an ERROR log, but no longer prevents startup. Remove it and add `"quota"` only to the arrays where it should appear; omit it everywhere to disable quota reads. Quota and Git are not selected by default.

Quota is independent of progress mode, but selecting it only in disabled Progress does not enable reads. Available quota may appear after a short task without delaying completion.

Replies support Markdown and split long text automatically. Large tables may use a plain-text layout; formatting unsupported by Telegram falls back to readable text.

#### Native provider quota

Selecting `quota` enables native Claude (`anthropic`) or OpenAI Codex (`openai-codex`) quota in the selected headers, independently of progress mode. It shows recognized 5h/7d used percentages and any approximate reset times from OMP, without a reading timestamp. Account identities, emails, credential IDs and raw reports are not sent to Telegram or stored in logs or the database.

Quota refreshes are best-effort while OMP is idle; queued prompts and controls take priority. Busy or long-running tasks show a cached value when available, otherwise omit the field. Cached values may be stale; failed or empty reads retain the last good value. Successful refreshes may update the latest final header if it selects `quota`, without changing or resending the answer or its Git result. Restarting the service clears the cache.

Unknown or ambiguous account information omits quota rather than showing another account's value. Native quota cannot identify which account was billed for each request when credentials rotate. For a configured sub2api provider, the [upstream quota source](#sub2api-upstream-quota) is used instead. See [architecture](doc/architecture.md#native-provider-quota) for native account and refresh mechanisms.

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

`storage.database_retention_days` controls retention of terminal bridge messages, eligible attachment caches and expired Follow-up request metadata. Default: `90` days; `0` disables automatic retention cleanup. Pending work and records still needed for delivery or progress cleanup may be retained longer. Workspace files, OMP session data, and saved conversation bindings are not removed. Retention does not delete delivered Telegram replies.

Before upgrading, stop the service and back up the data directory, workspaces, and OMP's own session storage. The bridge database alone is not a complete backup of OMP conversations.

Authorize trusted users only. The service runs with the filesystem permissions and environment of its OS user, and group members may see prompts and replies even when they cannot control the bot. Keep the bot token private.

## Logging

Logs support `text` and `json`, selected by `logging.format`. `logging.level` sets the global threshold; `[logging.component_levels]` overrides it for `daemon`, `bridge`, `rpc`, `telegram`, `store`, or `media`. See the annotated [configuration example](config.toml).

Logs are written to stderr; use your process manager for storage and rotation. Prompts, model replies, credentials, tool payloads, and workspace/session paths are not logged. Chat and thread IDs may still identify conversations, so restrict log access.

`rpc_queue_overflow` ERROR and `rpc_consumer_slow` WARN indicate RPC pressure or slow bridge operations. Check `/status` and the workspace before resending interrupted or uncertain work; it is not replayed automatically. Preserve nearby logs for diagnosis, but these warnings alone do not establish the cause. See the [architecture logging contract](doc/architecture.md#logging-contract) for diagnostic field interpretation.

## Development

For contributor workflows, verification and release recipes, see [Development, verification and release](doc/architecture.md#development-verification-and-release).

## Architecture

See [Architecture](doc/architecture.md) ([中文](doc/architecture.zh.md)) for implementation, failure handling and verification details.
