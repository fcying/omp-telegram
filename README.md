# omp-telegram

[中文版](README.zh.md)

Use [Oh My Pi](https://github.com/can1357/oh-my-pi) from Telegram private chats and topics. Each ordinary private chat or topic runs a separate omp session in a working directory you choose. Messages queue within a conversation; different conversations can work concurrently.

Supports text conversations, images and files, session selection, and restoring active sessions after a service restart. Your existing omp model, credentials, tools, and session history remain managed by omp.

## Requirements

- Linux, on amd64 or arm64.
- An omp installation supporting RPC protocol v2 and its runtime, such as Bun. Configure models and authentication for the same user that will run this service.
- Access to Telegram and your model provider.
- A Telegram bot. Ordinary private chats, private-chat topics, and group topics are supported; groups without a topic are not.

Only one polling service may use a bot token at a time. Do not run it alongside a webhook or another `getUpdates` consumer.

## Install

### Download a release

Download the archive for your architecture from [Releases](https://github.com/fcying/omp-telegram/releases). For amd64:

```sh
mkdir -p "$HOME/tool/omp-telegram"
tar -xJf omp-telegram-linux-amd64.txz -C "$HOME/tool/omp-telegram"
```

For arm64, use `omp-telegram-linux-arm64.txz`. Archives contain the binary and LICENSE; omp itself is installed separately. `SHA256SUMS` is provided with each release.

Development builds are published as the [Development build](https://github.com/fcying/omp-telegram/releases/tag/nightly). It is a prerelease and may be unstable.

### Build from source

Requires Go as specified in [go.mod](go.mod) and [just](https://github.com/casey/just):

```sh
git clone https://github.com/fcying/omp-telegram.git
cd omp-telegram
just install
```

This installs the binary to `~/tool/omp-telegram/omp-telegram`. Use `just install /your/bin/directory` to choose another directory. Configuration and data are not copied. Without just, build with `go build -o omp-telegram ./cmd/omp-telegram`.

## Quick start

### 1. Prepare Telegram

1. Create a bot with [@BotFather](https://t.me/BotFather) and keep its token private.
2. For an ordinary private chat, open the bot and press Start; no topic mode is required. To use topics instead, enable the bot's topic/threaded mode for private chats, or add the bot to a topic-enabled group and allow it to send messages. Create a topic yourself; this service does not create topics.
3. For groups, ensure the bot receives ordinary messages, not just commands. Disable privacy mode with BotFather's `/setprivacy`, following any instructions to re-add the bot, or grant the necessary administrator permissions.
4. Obtain your numeric user ID and chat ID. Before starting this service, use a Bot API client you control to inspect `message.from.id` and `message.chat.id` in [getUpdates](https://core.telegram.org/bots/api#getupdates). Never give your bot token to an ID lookup website. Send as your personal account, not an anonymous administrator or channel.

For the simplest setup, use an ordinary private chat with the bot. Topics are optional for private use and let you run multiple independent OMP sessions or projects in one Telegram chat. In groups, topics are required.

### 2. Prepare the configuration

Save the default configuration below as `~/tool/omp-telegram/config.toml`, beside the binary. You can also copy the repository's [config.toml](config.toml) and adjust it as needed:

```toml
[telegram]
token = "${OMP_TELEGRAM_BOT_TOKEN}"
allowed_users = ["${OMP_TELEGRAM_ALLOWED_USERS}"]
allowed_chats = ["${OMP_TELEGRAM_ALLOWED_CHATS}"]
# Telegram live task progress.
progress_mode = "summary"

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

If no configuration file is specified and the default file is absent, the service uses these embedded defaults without generating a file.
`storage.database_retention_days` enables the Database Message Retention janitor. It retains terminal inbox/outbox records for the configured number of days measured from their latest state transition. The default is `90`; set it to `0` to disable cleanup. Pending/submitted inbox and pending/sending outbox records remain durable. It never removes bindings, history, startup intents, workspaces, omp session files, or other omp data.

`telegram.progress_mode` controls one best-effort, editable live task message: `off` disables it and typing actions, `summary` shows assistant output, active tool names, and task state, and `verbose` also shows the six most recent observable tool activities. A live root task includes a Stop button that cancels only that active task by sending native `abort`; unlike `/stop`, it preserves bridge-deferred prompts so they run in order after the cancellation completes. The button is fenced to its owner and active task. For a normal `done` task, the live progress message remains until every durable final reply is confirmed delivered, then it is deleted best-effort. Progress for `cancelled` and `uncertain` tasks is retained. A confirmed non-retryable Telegram deletion rejection releases the durable progress association. When terminal data reaches the database retention cutoff without pending or sending outbox work, retention may drop only the local association and leave the Telegram progress message in place. Transport failures, 429, 5xx, and uncertain responses retain the association for a later retry. The message ID is persisted only as bridge delivery metadata; progress content is not duplicated and does not affect final reply reliability. Progress replies to the root user message when possible. It never includes reasoning, tool arguments, command text, results, stdout, or stderr.

Progress and its Stop button are sent on the regular worker tick after the three-second initial delay for a new task. Existing-message updates, typing, and durable final replies retain their independent behavior. `/stop` remains available while the first progress request is pending.

Missing terminal RPC completion is not treated as success: after at least 30 seconds without activity, the bridge requires two explicit native idle observations, at least 30 seconds apart, before durably marking the task `uncertain`. It never replays that task. Recovery stops typing, clears task controls, and retires the old process before queued work lazily resumes the same saved session. Compaction, retry, tools, host requests, and native UI waits prevent this recovery; missing state fields and probe errors are not idle evidence. This safety watchdog is independent of `worker.idle_timeout`.

An explicit `agent_end` with `isTerminal=false` disables watchdog recovery while waiting for native asynchronous continuation, even if native state reports idle for minutes. Only a subsequent `agent_start` re-enables recovery for a running turn; task completion or replacement clears the wait. A lost continuation cannot safely be distinguished from pending background work and is not automatically recovered.

`worker.idle_timeout` releases an otherwise idle omp process while retaining the validated session identity and its restoration eligibility. It defaults to `30m`; set `0` or `disabled` to turn it off. The bridge never releases a runtime with a task, queued or preparing prompt, compaction, handoff, session-list request, host request, startup transition, or a runtime-bound confirmation for model, thinking, fast, compact, new, or native UI choices; those confirmations keep the runtime active until they are consumed or expire. The independent `/resume` picker does not block release and may remain actionable after the runtime is released. The next prompt or native control that requires OMP state resumes that exact session before it is accepted. `/status` shows the retained binding with unavailable live metrics and reports `Idle: n/a` when the runtime has not been connected in the current generation. After release, `/stop` only clears bridge prompts and does not send `abort`; `/close` removes the saved restoration eligibility without starting omp.

### 3. Set the environment and start

Set the variables referenced by the configuration. Replace the example token and IDs with your own:

```sh
export OMP_TELEGRAM_BOT_TOKEN='your-bot-token'
export OMP_TELEGRAM_ALLOWED_USERS=123456789
export OMP_TELEGRAM_ALLOWED_CHATS=123456789

~/tool/omp-telegram/omp-telegram --check
~/tool/omp-telegram/omp-telegram
```

In a private chat, the chat ID matches your user ID. For a group, use its negative chat ID, such as `-1001234567890`. Multiple IDs may be comma-separated. Both the user and chat allowlists must match.

Keep the token private. Typing it directly into a shell may leave it in command history; protect any startup file containing it and do not commit it to Git.

`--check` validates local configuration, finds the omp executable, and creates the configured data/workspace directories; it does not test Telegram or model authentication. The second command runs in the foreground; Ctrl-C stops the service.

### 4. Start a conversation in a private chat or topic

```text
/new demo
```

With the default workspace root, this creates or opens `workspace/demo` beside the installed binary and starts a new omp session there. You can instead select a directory on the machine running the service:

```text
/new /home/you/projects/my-app
```

Wait for the ready message, then send ordinary text. Missing directories are created; existing files are not copied or cleared.

For a first session, plain `/new` uses `storage.workspace_root` itself (by default, `workspace/` beside the executable). Later uses keep this conversation's last selected directory. Different conversations using the default have independent sessions but share files; use `/new <project-name>` for separate project directories.

## Conversation commands

| Command | What it does |
| --- | --- |
| `/new <name or path>` | Start a fresh session in the selected directory. Replacing a running instance requires confirmation |
| `/new` | Start a fresh session in the previous directory, or `storage.workspace_root` if none was selected |
| `/stop` | Stop the current task and clear queued prompts, keeping the session open; a released session only clears prompts |
| `/close` | Close the current logical session, preserving files and session history without waking a released runtime |
| `/resume` | Choose a saved omp session in the current directory using paginated buttons |
| `/resume <session ID>` | Replace the current logical session with a native omp session and its original directory |
| `/status` | Show workspace, session title/ID, model, thinking, fast mode, context usage, activity, queue, and speed |
| `/name <title>` | Name the current omp session, e.g. `/name Bugfix HAL`; does not rename the Telegram topic |
| `/model` | Choose a configured OMP cycle role with buttons; shows the current model |
| `/model provider/model` | Switch models while idle |
| `/thinking` | Choose the thinking level while idle; shows the current level |
| `/fast [on\|off\|status]` | Choose fast mode with buttons, explicitly enable/disable it, or show setting and actual activity |
| `/compact` | Compact context while idle, after confirmation |
| `/handoff [instructions]` | Run OMP's native handoff while idle with an empty queue; optional instructions guide the handoff |
| `/review [arguments]` | Run omp's native `/review` command as an independent task |
| `/help` | Show help |

All commands above, including `/new <name or path>` and `/resume`, work in ordinary private chats as well as topics. An ordinary private chat uses `(chat, 0)` with the same worker and session lifecycle as a topic; no separate private-chat worker or database migration is needed. `/followup` remains unsupported.

Ordinary text, attachments, and `/review` are independent tasks queued by the bridge and run sequentially within each conversation. Messages sent while a task is running wait for it to finish. `/stop` clears the bridge queue, then sends a plain `abort` request only when OMP is connected; a released runtime has no native task and is not started for `/stop`.

The model picker follows OMP's `cycleOrder` roles, such as `smol`, `default`, and `slow`, rather than listing every available model. OMP resolves the selected role and its thinking setting. Opening the menu does not switch models; selecting requires an idle instance with an empty queue. Buttons disappear after selection, and the reply reports the actual selected model. Manual `/model provider/model` remains available.

The role picker supports `omp.args` with `--config path` or `--config=path`, including multiple files in their original order. The native query loads inherited `PI_CONFIG_FILES` first, followed by these overlays; relative paths are resolved from the worker workspace. It does not rewrite configuration files. Runtime `--profile`, `--smol`, `--slow`, and `--plan` overrides still require explicit `/model provider/model` selection.

`/thinking` offers the fixed levels `off`, `minimal`, `low`, `medium`, `high`, `xhigh`, and `max`. The menu marks the current effective level; OMP may adjust the requested level for the current model. The bridge does not send the undocumented `auto` value through `set_thinking_level`. Existing OMP configuration and model-role thinking settings remain unchanged.

Bot menus, buttons, and service messages are in English. You can write prompts in any language; model replies are not translated by the service.

### Restart and recovery

- Restarting the service restores sessions that were still open. Conversations closed with `/close` stay closed; `/stop` does not disable restoration.
- Interrupted tasks are not rerun, and pending tasks, including `/review`, are canceled rather than replayed automatically. Check the conversation and files before deciding to resend a request.
- A missing session file or working directory causes recovery to fail, not to create a replacement session. Startup or session-switch interruptions may require manual `/resume`.
- **Send `/close` before deleting a topic.** Deleting a Telegram topic does not automatically stop its omp instance.

## Images and files

Send photos or documents with Telegram's attachment button. Add a caption to tell omp what to do. Without one, omp is asked to inspect the attachment. Albums are processed as separate messages.

To receive a file, ask naturally, for example: "Send me the report as a file." omp can return regular files from the current working directory. A message saying an attachment is queued does not mean it has arrived; check for the actual attachment.

| Transfer | Limit |
| --- | --- |
| Download from Telegram | 20 MB |
| Send a document | 50 MB |
| Send a photo | 10 MB, JPEG or PNG |

Incoming originals remain under `.telegram/incoming/` in the working directory. Large images may be represented by a preview or a local file path. Image understanding depends on the model, and document reading depends on its available tools. Stopping a task does not delete originals already submitted to omp.

## Configuration

Settings are loaded in this order:

1. The file selected by `--config <path>` or `-c <path>`.
2. Otherwise, `config.toml` beside the real executable, after resolving symlinks.
3. If that default file is absent, the embedded configuration. No file is generated.

An explicitly selected missing file, an unreadable file, or invalid TOML causes startup to fail. The grouped default configuration above includes the required allowlists.

Configuration uses grouped TOML tables: `[telegram]`, `[omp]`, `[storage]`, `[worker]`, `[logging]`, and optional `[logging.component_levels]`. Flat root fields, the former `[log_component_levels]` table, fields in the wrong table, and mixed flat/grouped layouts are rejected. This is an intentional breaking cutover: existing private configuration files must be migrated manually before upgrading; the service does not rewrite them automatically.

| Setting | Purpose |
| --- | --- |
| `telegram.token` | Bot token; prefer an environment reference |
| `telegram.allowed_users`, `telegram.allowed_chats` | Required numeric ID allowlists; literal integers or comma-separated environment values |
| `telegram.progress_mode` | Live task progress: `off`, `summary`, or `verbose`; default `summary` |
| `omp.binary` | Executable name found through `PATH`, or an absolute path; not a shell command |
| `omp.args` | Extra omp arguments; defaults to optional `OMP_TELEGRAM_ARGS`. An explicit empty string disables them |
| `storage.data_dir` | Database, lock, and outgoing attachment storage; defaults to the executable directory |
| `storage.workspace_root` | Base directory for `/new <name>`; defaults to optional `OMP_TELEGRAM_WORKSPACE_ROOT`, then executable-directory `workspace/` |
| `storage.database_retention_days` | Terminal database message retention in days; default `90`, or `0` to disable cleanup |
| `worker.max_workers` | Maximum active OMP processes, default 4 |
| `worker.queue_capacity` | Waiting prompts per conversation, default 16 |
| `worker.idle_timeout` | Idle duration before releasing an otherwise quiescent OMP process, default `30m`; set `0` or `disabled` to turn it off |
| `logging.level` | Global structured-log threshold: `debug`, `info`, `warn`, or `error`; default `info` |
| `logging.format` | Structured-log encoding, `text` or `json`; default `text` |
| `[logging.component_levels]` | Optional per-component thresholds for `daemon`, `bridge`, `rpc`, `telegram`, `store`, and `media` |

Strings support `$VAR` and `${VAR}`; use `$$` for a literal dollar sign. Every string in the grouped tables uses the same expansion rules after TOML parsing and is expanded exactly once. The default references to `OMP_TELEGRAM_ARGS` (`omp.args`) and `OMP_TELEGRAM_WORKSPACE_ROOT` (`storage.workspace_root`) may be unset; other missing references, including the token and required allowlists, fail. `.env` files and shell startup files are not loaded automatically.

Structured logging is configured only in TOML; there are no dedicated logging environment variables. The normal `$VAR`, `${VAR}`, and `$$` expansion rules apply to `logging.level`, `logging.format`, and values in `[logging.component_levels]`. Component names are validated against the six supported names before override values are expanded. Text mode uses compact `YYYY-MM-DD HH:MM:SS LEVEL [component] message key=value` lines, preserving the original message and all remaining structured attributes without `time=`, `level=`, `msg=`, or `component=` header labels. Strings and control characters are escaped as needed to keep each record on one line. JSON mode remains standard `slog.JSONHandler` output, one independently parseable object per line including the `component` field. The component table overrides `logging.level` only for the named component; unspecified components inherit the global level.

Text example:

```text
2026-09-19 13:20:01 WARN [telegram] telegram polling failed event=poll_failed reason=timeout
```

`--version` and successful `--check` output remain unchanged. Configuration and CLI errors that occur before the logger registry is created remain ordinary human-readable text, even when `logging.format = "json"`. After registry creation, daemon logs, including lock and database startup failures, use the selected format on stderr. The process manager remains responsible for log storage and rotation.

To select another bridge configuration:

```sh
~/tool/omp-telegram/omp-telegram -c "$HOME/.config/omp-telegram/config.toml" --check
```

### Pass options to omp

For example, to use an omp configuration overlay you have prepared:

```sh
export OMP_TELEGRAM_ARGS="--config \"$HOME/.config/omp/telegram.yml\""
```

This configures **omp**, not the bridge. You can also set `omp.args` in TOML. Arguments support quoting but are passed directly without a shell; use absolute paths for configuration files. The bridge does not change omp's configuration, credentials, tools, or approval policy automatically. RPC mode, working directory, and session lifecycle options are reserved for the bridge.

Restart the service after changing configuration or its environment. For background operation, use your preferred process manager and explicitly provide the environment and a `PATH` containing both omp and its runtime.

## Data, upgrades, and safety

By default, runtime data stays beside the installed binary:

```text
~/tool/omp-telegram/
├── omp-telegram
├── config.toml          # Optional
├── omp-telegram.db
├── daemon.lock
└── workspace/
```

**Relative `storage.data_dir` and `storage.workspace_root` paths are based on the executable directory**, not the launch directory or configuration file's location. Moving the binary can therefore select a different database. Use absolute paths when keeping data elsewhere. An explicit relative `--config` path is the exception: it is caller-relative.

Before upgrading, stop the service and back up its data directory, working directories, and omp's own session storage. The bridge database alone is not a backup of omp conversations. Do not delete SQLite's `-wal`/`-shm` files or copy only the main database while it is being written. Older unversioned development databases are not automatically upgraded; back them up and use a fresh data directory if startup reports an unsupported schema.

Check the installed application version with `~/tool/omp-telegram/omp-telegram --version` or `-v`.

- Authorize trusted users only. omp runs with the service user's filesystem permissions and environment. Separate conversation sessions are not a filesystem or credential sandbox.
- Group members may see prompts and replies even when they cannot control the bot. The database stores message content and applies the configured `storage.database_retention_days` policy.
- A send timeout may still mean a message arrived. Do not assume a missing reply means the task did not run.
- Prefer normal shutdown over `kill -9`; forced termination does not guarantee that every tool subprocess exits.

## Troubleshooting and limitations

| Symptom | Check |
| --- | --- |
| Bot does not respond | Both allowlists, a topic when using a group, group privacy settings, and whether another poller or webhook is using the bot |
| `omp executable not found` | The service process's `PATH`, including omp and its runtime; your interactive shell may have a different environment |
| `/resume` shows no sessions | The selected directory and whether omp has saved history yet. A new empty session may not have a resumable file |
| `/compact` fails | Short sessions may have nothing to compact. Check omp's model configuration if compaction also fails locally |
| Unsupported database schema | Back up the database and use a supported database or fresh data directory; do not change the version number by hand |

Voice/transcription, automatic topic creation, and arbitrary terminal input/editor dialogs are not supported. Some confirmation/selection prompts can use Telegram buttons, but not every interactive tool approval is available remotely. Automatic approval is never enabled by the bridge; verify the approval workflows you rely on before leaving a session unattended.

## Roadmap

The following features are planned, not currently implemented.

- List saved conversation/session bindings
- Bridge queue status and cancel individual pending tasks
- Telegram reply context
- Telegram media group / album support
- Session export
- Resume favorites / pinned sessions
- `/doctor` diagnostics

### Waiting for upstream OMP support

- Reliable active-turn steering
- Native follow-up support
- Native queue clear / atomic abort-and-clear

For contributors: [architecture and development](doc/architecture.md).
