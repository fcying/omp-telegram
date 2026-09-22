# omp-telegram

[English](README.md) | 中文

`omp-telegram` 是 [Oh My Pi](https://github.com/can1357/oh-my-pi) 的 Telegram bridge.

它让 Telegram 对话使用持久化的 OMP session, 主要提供:

- 服务重启后的 session 恢复
- 流式任务进度和 Stop 控件
- 文字及附件收发
- model, thinking 和 fast-mode 控制
- 持久化的最终回复交付
- 结构化日志

每个普通私聊或 topic 都有自己的 OMP session. 普通文字, 附件和 `/review` 在同一对话内按顺序执行; 不同对话可以并行工作.

## 运行要求

- Linux, amd64 或 arm64.
- 已安装支持 RPC protocol v2 的 omp 及其运行时, 例如 Bun. 使用运行服务的同一用户配置模型和认证.
- 能访问 Telegram 和模型服务.
- 一个 Telegram bot. 支持普通私聊, 私聊 topic 和群组 topic; 不支持没有 topic 的群组消息.

同一个 bot token 同时只能由一个 polling 服务使用. 不能与 webhook 或其他 `getUpdates` 消费者同时运行.

## 安装

### 下载发布版

从 [Releases](https://github.com/fcying/omp-telegram/releases) 下载对应架构的压缩包. amd64 示例:

```sh
mkdir -p "$HOME/tool/omp-telegram"
tar -xJf omp-telegram-linux-amd64.txz -C "$HOME/tool/omp-telegram"
```

arm64 使用 `omp-telegram-linux-arm64.txz`. 压缩包包含二进制和 LICENSE, omp 需要单独安装. 每个发布版都附带 `SHA256SUMS`.

开发构建发布在 [Development build](https://github.com/fcying/omp-telegram/releases/tag/dev). 这是 prerelease, 可能不稳定.

### 从源码构建

需要 [go.mod](go.mod) 指定的 Go 版本及 [just](https://github.com/casey/just):

```sh
git clone https://github.com/fcying/omp-telegram.git
cd omp-telegram
just install
```

默认安装到 `~/tool/omp-telegram/omp-telegram`. 可用 `just install /your/bin/directory` 指定其他目录. 不复制配置或数据. 不使用 `just` 时执行:

```sh
go build -o omp-telegram ./cmd/omp-telegram
```

## 配置

将 [config.toml](config.toml) 复制到二进制旁, 或创建包含以下设置的配置文件:

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

### 配置文件和环境变量

配置按以下顺序选择:

1. 通过 `--config <路径>` 或 `-c <路径>` 指定的文件.
2. 解析符号链接后的真实二进制所在目录中的 `config.toml`.
3. 默认文件不存在时使用内嵌默认配置.

显式指定的文件不存在, 文件不可读或 TOML 无效时, 启动都会失败. 配置必须使用上面示例中的分组 table; 旧的 flat 字段和 flat/grouped 混合布局不再接受.

字符串支持 `$VAR` 和 `${VAR}`, `$$` 表示字面美元符号. TOML 解析后每个环境变量引用只展开一次. 默认引用的 `OMP_TELEGRAM_ARGS`, `OMP_TELEGRAM_PROGRESS_MODE` 和 `OMP_TELEGRAM_WORKSPACE_ROOT` 可以未设置. `telegram.progress_mode` 为空或非法时回退到 `summary`. token 和白名单必须提供. 程序不会自动加载 `.env` 文件.

`telegram.allowed_users` 和 `telegram.allowed_chats` 都是必填项. 只有发送者和 chat 同时命中白名单时才接受 update. 普通私聊的 chat ID 通常等于 user ID; 群组 chat ID 通常是负数.

### 配置项

| 配置项 | 说明 |
| --- | --- |
| `telegram.token` | Telegram bot token. 推荐引用环境变量. |
| `telegram.allowed_users`, `telegram.allowed_chats` | 必填的数字 ID 白名单. 可直接填写整数, 或引用逗号分隔的环境变量值. |
| `telegram.progress_mode` | 任务实时进度: `off`, `summary` 或 `verbose`. 默认读取 `OMP_TELEGRAM_PROGRESS_MODE`; 为空或非法时回退到 `summary`. 见 [Progress UI](#progress-ui). |
| `omp.binary` | 从 `PATH` 查找的可执行文件名或绝对路径, 不是 shell 命令. |
| `omp.args` | OMP 额外参数. 默认读取可选的 `OMP_TELEGRAM_ARGS`; 显式空字符串禁用额外参数. |
| `storage.data_dir` | 数据库, lock 和待发送附件的存储目录. 默认是二进制所在目录. |
| `storage.workspace_root` | `/new <名称>` 使用的根目录. 默认读取可选的 `OMP_TELEGRAM_WORKSPACE_ROOT`, 再回退到二进制旁的 `workspace/`. |
| `storage.database_retention_days` | 终态 bridge 消息 metadata 的保留天数. 默认 `90`; `0` 关闭自动清理. |
| `worker.max_workers` | 同时连接的 OMP 进程上限. 默认 `4`. |
| `worker.queue_capacity` | 每个对话最多等待的任务数. 默认 `16`. |
| `worker.idle_timeout` | 保留 session 的同时释放持续空闲 OMP 进程前的时长. 默认 `30m`; `0` 或 `disabled` 关闭. |
| `logging.level` | 全局日志级别: `debug`, `info`, `warn` 或 `error`. 默认 `info`. |
| `logging.format` | 日志格式: `text` 或 `json`. 默认 `text`. |
| `[logging.component_levels]` | 可选的组件级别覆盖: `daemon`, `bridge`, `rpc`, `telegram`, `store` 和 `media`. |

`omp.args` 支持用于分组参数的 shell-style quoting, 但参数直接传递, 绝不由 shell 执行. OMP overlay 仍然属于 OMP 配置; bridge 不会自动修改模型, 凭据, 工具或审批策略.

修改配置或环境变量后需重启服务. 后台运行时请使用进程管理器, 明确提供环境变量, 并确保 `PATH` 同时包含 omp 及其运行时.

## 使用

### 启动服务

1. 在 [@BotFather](https://t.me/BotFather) 创建 bot, 私下保存 token.
2. 普通私聊打开 bot 并点击 Start. 使用 topic 时, 为私聊启用 topic 模式, 或把 bot 加入启用了 topic 的群组. topic 需要自行创建, 服务不会创建.
3. 群组中的 bot 必须能收到普通消息, 不只是命令. 可能需要通过 BotFather 的 `/setprivacy` 设置或授予合适的管理员权限.
4. 使用自己控制的 Bot API 客户端获取数字 user ID 和 chat ID. 不要把 bot token 交给第三方 ID 查询网站.

设置配置引用的环境变量, 然后检查并启动服务:

```sh
export OMP_TELEGRAM_BOT_TOKEN='your-bot-token'
export OMP_TELEGRAM_ALLOWED_USERS=123456789
export OMP_TELEGRAM_ALLOWED_CHATS=123456789
# 可选: off, summary 或 verbose.
export OMP_TELEGRAM_PROGRESS_MODE=summary

~/tool/omp-telegram/omp-telegram --check
~/tool/omp-telegram/omp-telegram
```

`--check` 检查本地配置, 查找 omp 可执行文件, 并创建配置中的数据及工作目录. 它不验证 Telegram 或模型认证. 最后一条命令前台运行服务, Ctrl-C 退出.

### 开始 session

使用 `/new` 加项目名或路径:

```text
/new demo
/new /home/you/projects/my-app
```

目录不存在时会创建命名 workspace. 已有文件不会复制或清空. 直接发送 `/new` 时, 优先使用当前对话之前的 workspace; 没有选择过时使用 `storage.workspace_root`. 等收到就绪消息后再发送普通文字.

### 对话命令

| 命令 | 用途 |
| --- | --- |
| `/new <名称或路径>` | 在指定目录开启新 session. 替换正在运行的 session 前需要确认. 对于没有 binding 的 Telegram topic, Telegram topic 标题会同步为解析后 workspace 的最后一级目录名; 后续替换 session 不会重命名 topic. |
| `/new` | 在上次目录开启新 session; 没有历史目录时使用 `storage.workspace_root`. |
| `/stop` | 中止当前任务并清空排队任务, 保留 session. released session 只清空排队任务. |
| `/queue` | 查看当前对话的运行状态和 bridge 待执行队列. 每个 pending task 都有独立 Cancel 按钮; 不会中止 active task. 队列只存在于 runtime; daemon shutdown 会取消 pending task, 不会恢复. |
| `/close` | 关闭当前 session, 保留文件和 OMP history. |
| `/bindings` | 列出当前 Telegram chat 的已保存 conversation/session binding, 并在可用时显示原生 session name. Pending, open 和当前对话不能删除; 其他 topic 的 closed binding 确认后可删除, 只删除 bridge metadata, 不删除 workspace 或 OMP 原生 history. |
| `/resume` | 从当前 workspace 的已保存 session 中选择要恢复的 session. 当前 conversation 和 workspace 的 pinned session 会排在前面; 每行都有 Pin 或 Unpin 按钮. |
| `/resume <session ID>` | 恢复原生 OMP session 及其原目录. |
| `/export` | 从当前 workspace 打开原生 OMP session 导出 picker. 默认导出原始 main session JSONL; 源文件会复制到私有 attachment outbox, 保留经过安全处理的原文件名, 并受 50 MB document 限制. 当前 session 在 task 或队列活跃时会直接拒绝, 空闲后需要重新执行 `/export`. |
| `/export html` | 打开原生 OMP HTML 导出的 picker. bridge 会先把选中的 main session JSONL snapshot 到私有 spool, 再从这个稳定 snapshot 调用原生 exporter; 不包含 companion 或 subagent transcript. 结果文件名为 `omp-session-<short-id>.html`; exporter 超时为 30 秒, 输出增长受 50 MB document 限制. |
| `/export <session ID>` | 不打开 picker, 直接导出指定 session 的原始 OMP main session `.jsonl`. 使用 `/export html <session ID>` 显式选择 HTML. |
| `/status` | 查看 workspace, session, model, thinking, fast, context, 活动状态, 队列和速度. |
| `/doctor` | 异步运行安全的 bridge 诊断, 不启动或修改 OMP session. |
| `/name <名称>` | 命名当前 OMP session. 不修改 Telegram topic 名称. |
| `/model` | 用按钮选择 OMP 配置的 model role. |
| `/model provider/model` | 空闲时切换到指定 model. |
| `/thinking` | 空闲时选择 thinking level. |
| `/fast [on\|off\|status]` | 选择 fast mode, 显式开启或关闭, 或查看状态. |
| `/compact` | 空闲时经确认压缩当前 context. |
| `/handoff [补充要求]` | 空闲且队列为空时运行 OMP 原生 handoff. |
| `/review [arguments]` | 作为排队任务运行 OMP 原生 `/review` 命令. |
| `/help` | 查看帮助. |

普通文字, 附件和 `/review` 都按对话排队并串行执行. 任务运行期间发送的消息会等待当前任务结束, 不会打断活动任务. 不同对话可以并行工作, 受 worker 配置上限影响.

`/doctor` 检查运行时配置, Telegram `getMe`, SQLite 健康状态, data directory 写入能力, 配置的 OMP binary, 当前 workspace 和保存的 session, runtime 状态, 不确定的 inbox/outbox 记录以及磁盘剩余空间. 只返回固定的安全摘要, 不包含 token, header, prompt, 原始 RPC state 或完整本地路径. 检查期间如果 conversation state 发生变化, 结果会丢弃.

以上命令都可用于普通私聊和 topic. bot 菜单, 按钮和服务提示使用英语; 可以用任意语言提问, bridge 不翻译模型回复.

### Session 导出

`/export` 会列出当前 workspace 中保存的 OMP session, 并将选中的原生 `.jsonl` session 文件发送到 Telegram. 默认格式是 OMP 原始 main-session JSONL. `/export html` 则将选中的 session 导出为 standalone HTML viewer.

也支持直接指定 session:

```text
/export <session-id>
/export html <session-id>
```

要在另一台电脑上使用原生 JSONL 导出, 准备对应的源码目录, 下载文件, 然后在目标项目目录执行:

```sh
omp --resume /path/to/session.jsonl
```

如果 session 中记录的旧工作目录已经不存在, OMP 可能要求将 session re-root 到当前目录. Session 导出不包含 workspace 源码文件, 源码树, Git 状态, 未提交文件, credentials, OMP 配置或 shell environment. 项目文件需要单独通过 `git clone`, `git pull`, `scp`, `rsync` 或其他方式同步.

导出的 session 文件可能包含敏感的 conversation, tool, command 和 path 数据, 包括 prompts, assistant responses, tool calls, tool results, 本地路径, 命令输出, 源码片段以及意外出现在 transcript 中的 secrets. 只应将这些文件发送到可信的 Telegram 对话. 不会额外要求二次确认; 用户输入 `/export` 本身就是明确确认.

### 常见问题与限制

| 现象 | 检查项 |
| --- | --- |
| bot 没有反应 | 两个白名单, 群组 topic, 群隐私设置, 以及是否有其他 polling 服务或 webhook 占用 bot. |
| `omp executable not found` | 服务进程的 `PATH`, 包括 omp 及其运行时. 它可能不同于交互式 shell. |
| `/resume` 列表为空 | 当前选择的目录以及 OMP 是否已经保存历史. 新 session 可能还没有可恢复文件. |
| `/compact` 失败 | 短 session 可能没有可压缩内容. 如果本机 OMP 也失败, 检查其 model 配置. |
| 数据库结构不支持 | 备份数据, 使用受支持的数据库或新数据目录; 不要手动修改版本号. |

不支持语音/转写, 自动创建 topic, 以及任意终端或编辑器对话框. 部分确认和选择流程可以使用 Telegram 按钮, 但不是所有交互式工具审批都能远程完成. bridge 从不自动批准工具操作.

## Progress UI

`telegram.progress_mode` 控制实时任务状态消息.

支持的值:

- `off` - 关闭 progress 消息和 typing action.
- `summary` - 显示 assistant 输出, 活动工具名和任务状态.
- `verbose` - 另外显示最近的可观察工具活动.

新任务的 progress 会延迟约三秒, 因此短任务通常只发送最终回复. 活跃任务带有 **Stop** 按钮. 它只中止当前任务; `/stop` 还会清除对话中已经排队的任务, 按钮则让这些任务在取消完成后继续执行.

`/queue` 只取消选中的 bridge pending task. 不管理 OMP native queue, 不调整顺序, 不提供中止 active task 的 Stop 按钮, 也不会在 daemon shutdown 后持久化或恢复 pending task.

Progress 是 best-effort UI, 不影响最终回复的持久化交付.

Progress 不显示模型 reasoning, 原始工具参数或结果, 命令文本以及进程输出.

## 附件

使用 Telegram 附件按钮发送照片或文档, 在 caption 中说明要让 OMP 做什么. 没有 caption 时, 默认请 OMP 查看附件. Photo 和 document 相册会聚合为一个 OMP task; 第一条消息立即占用一个 bridge queue slot, 后续成员加入该任务而不会创建独立 prompt.

相册成员会先按 Telegram message ID 排序, 再下载. 一个相册最多接受 10 个成员. 只使用一次按顺序找到的第一个非空 caption; 如果全部为空, prompt 使用 `Please inspect the attached files.`. 按顺序找到的第一个带 reply context 的成员提供一次上下文, 最终回复定位到相册第一条消息. preparation 采用 all-or-nothing: 任一成员下载失败都会删除整个 incoming directory, 删除 owner task, 并只发送一次 album 失败提示. 如果相册收集被取消、拒绝或封存, 同一 media group 的迟到成员会在短暂抑制窗口内被消费, 不会重新创建任务.

需要回传文件时, 可以直接告诉 OMP, 例如: "把报告作为文件发给我". OMP 可以发送当前 workspace 中的普通文件. "附件已加入队列" 不代表已经送达, 请检查聊天中是否出现实际文件.

| 传输方向 | 限制 |
| --- | --- |
| 从 Telegram 下载 | 20 MB |
| 发送文档 | 50 MB |
| 发送照片 | 10 MB, JPEG 或 PNG |

收到的文件保留在所选 workspace 的 `.telegram/incoming/` 下. 大图可能以预览或本地文件路径提供给模型. 识图能力取决于模型, 文档读取能力取决于可用工具. `/stop` 不会删除已经提交给 OMP 的附件.

输出附件会在加入 Telegram 交付队列前复制到私有 delivery snapshot, 因此源文件之后的变化不会改变已经排队的内容.

## Telegram reply context

回复 Telegram 历史消息后发送普通文字、附件或 `/review`, bridge 会把一层引用上下文加入 OMP prompt. Telegram 非空 quote 优先; 否则按被回复文字、photo 标记、带文件名的 document 标记或仅 caption 的消息提取. 无法识别的被回复消息会明确标记, 但不会阻止当前 prompt.

上下文只标记 `From: bot` 或 `From: user`. 被回复的 photo 和 document 只作为描述, 本功能不会下载或重新导入历史附件, 也不会展开 reply chain. Telegram 最终回复仍定位到当前用户消息; reply context 只作为 OMP 输入.

Reply context 最多 3000 个 UTF-16 code units, 超限时追加 `...[truncated]`. 当前用户消息不会被截断. `/queue` 显示当前用户文字或附件 caption, 不显示给 OMP 的 synthetic wrapper.

Reply context 只来自当前 Update 解码出的 `ReplyToMessage` 和 `Quote`. 原始 update 保存在 `inbox.raw`; 不查询 Telegram 历史, 不新增历史 API 请求, 也不增加 reply-context 数据库列.

## Sessions and Recovery

已提交的 session 会在 daemon 重启后保留.

结果不确定的活动操作不会自动重放.

daemon 停止时仍在等待的任务会取消. 决定重发前先检查聊天和 workspace.

- `/close` 后 session 保持关闭; `/stop` 不会关闭 session, 后续仍可发送 prompt.
- 服务启动时如果已保存的 OMP session 文件或 workspace 不可用, 包括尚未持久化 history 的新 session, bridge 会跳过恢复并将 binding 保持为 closed, 提示使用 `/new`; 不会创建替代 session. 其他 session 切换失败时, 执行 `/close`, 再执行 `/new` 或 `/resume`.
- `worker.idle_timeout` 可能释放空闲 OMP 进程, 但只有在 OMP 已写入可恢复的 session 文件后才会释放. 新建 native session 在此之前可能保持 connected. 下一条 prompt 或 OMP 控制命令会恢复同一个 session.
- **删除 Telegram topic 前先发送 `/close`.** 删除 topic 不会自动停止对应的 OMP session.

每个对话有独立的 session, 但使用同一 workspace 的 session 会共享文件. 不同对话不是文件系统或凭据沙箱.
`/bindings` 列出当前 Telegram chat 的已保存 binding. Pending, open 和当前对话的删除按钮会禁用. 其他 topic 的 closed binding 可在确认后忘记; 只删除 bridge metadata 和 history snapshot, 不删除 workspace 或 OMP 原生 session history.

## 数据与保留

Bridge 状态保存在:

`<storage.data_dir>/omp-telegram.db`

`storage.data_dir` 默认是解析符号链接后的真实二进制所在目录. 相对 storage 路径使用该目录, 不使用启动目录. 需要把数据放在其他位置时请使用绝对路径.

终态 Telegram bridge 记录按 `storage.database_retention_days` 保留, 默认 90 天.

设置以下内容可关闭自动清理:

```toml
[storage]
database_retention_days = 0
```

Retention cleanup 不会删除:

- OMP session
- workspace
- 其他 OMP 数据

升级前先停止服务, 备份数据目录, workspace 以及 OMP 自己的 session 存储. 只备份 bridge 数据库不等于完整备份 OMP 对话. 不要删除 SQLite 的 `-wal` 或 `-shm` 文件, 也不要在服务运行时只复制主数据库.

只授权可信用户. 服务使用其 OS 用户的文件权限和环境变量运行; 群成员可能看到提问和回复, 即使没有控制 bot 的权限. 保护好 bot token.

## 日志

结构化日志支持:

- `text`
- `json`

全局日志级别:

```toml
[logging]
level = "info"
format = "text"
```

可选的组件级别:

```toml
[logging.component_levels]
bridge = "debug"
rpc = "debug"
telegram = "info"
```

支持的组件:

- `daemon`
- `bridge`
- `rpc`
- `telegram`
- `store`
- `media`

日志不会写入 raw prompt, model 回复, Telegram 凭据, raw RPC frame, API response body, tool 参数, workspace/session 路径, stdout 或 stderr. Chat 和 thread ID 仍可能识别具体对话, 应限制日志访问权限.

## 开发

```sh
just build
just test
just check
just install
just deploy
```

- `just build` 构建二进制.
- `just test` 运行单元测试, 不启动或重启服务.
- `just check` 运行测试, race 检查和 `go vet`, 不启动 Telegram polling.
- `just install` 只安装二进制, 配置和数据由用户管理.
- `just deploy` 安装二进制并重启已有的 supervisor daemon.

## 路线图

目前没有列出的 bridge-side 计划功能.

### 等待上游 OMP 支持

- 可靠的活动 turn steering
- 原生队列清空 / 原子 abort-and-clear

## 架构

实现细节和失败语义见:

- [中文架构说明](doc/architecture.zh.md)

架构文档覆盖:

- inbox / outbox 持久化
- Telegram 交付不确定性
- progress message 持久化
- 崩溃和重启恢复
- session 生命周期
- watchdog 行为
- 数据库 retention
- 附件 snapshot
- 结构化日志契约
