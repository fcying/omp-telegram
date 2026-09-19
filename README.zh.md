# omp-telegram

[English](README.md) | 中文

在 Telegram 私聊和 topic 中使用 [Oh My Pi](https://github.com/can1357/oh-my-pi). 每个普通私聊或 topic 单独运行一个 omp 会话, 工作目录由你选择. 同一对话的消息依次处理, 不同对话可以并行工作.

支持文字对话, 图片和文件收发, 会话选择, 以及服务重启后恢复原会话. 模型, 认证, 工具和会话历史继续使用你自己的 omp 配置.

## 运行要求

- Linux, amd64 / arm64 架构.
- 已安装支持 RPC protocol v2 的 omp 及其运行时, 如 Bun. 使用将要运行服务的同一个用户配置模型和认证.
- 能访问 Telegram 和模型服务.
- 一个 Telegram bot. 支持普通私聊, 私聊 topic 和群组 topic; 不支持没有 topic 的群组消息.

同一个 bot token 只能由一个 polling 服务使用, 不能同时保留 webhook 或另一个 `getUpdates` 消费者.

## 安装

### 下载发布版

从 [Releases](https://github.com/fcying/omp-telegram/releases) 下载对应架构的压缩包. 以 amd64 为例:

```sh
mkdir -p "$HOME/tool/omp-telegram"
tar -xJf omp-telegram-linux-amd64.txz -C "$HOME/tool/omp-telegram"
```

arm64 使用 `omp-telegram-linux-arm64.txz`. 压缩包包含二进制和 LICENSE, omp 需要单独安装. 每次发布附带 `SHA256SUMS` 校验文件.

开发构建发布在 [Development build](https://github.com/fcying/omp-telegram/releases/tag/nightly). 它是 prerelease, 可能不稳定.

### 从源码构建

需要 [go.mod](go.mod) 指定的 Go 版本及 [just](https://github.com/casey/just):

```sh
git clone https://github.com/fcying/omp-telegram.git
cd omp-telegram
just install
```

默认安装到 `~/tool/omp-telegram/omp-telegram`. 可用 `just install /your/bin/directory` 指定其他目录, 不复制配置或数据. 不使用 just 时, 可执行 `go build -o omp-telegram ./cmd/omp-telegram` 编译.

## 快速开始

### 1. 准备 Telegram

1. 在 [@BotFather](https://t.me/BotFather) 创建 bot, 私下保存 token.
2. 普通私聊只需打开 bot 并点击 Start, 不需要启用 topic 模式. 如需使用 topic, 私聊应开启 bot 的 topic/threaded mode; 群组应启用 topic, 加入 bot 并允许其发送消息. 请自行创建 topic, 本程序不会创建 topic.
3. 群组中的 bot 必须能收到普通消息, 不只是命令. 可通过 BotFather 的 `/setprivacy` 关闭隐私模式, 按提示重新加入 bot, 或授予所需的管理员权限.
4. 获取你自己的数字 user ID 和 chat ID. 启动服务前, 用自己控制的 Bot API 客户端查看 [getUpdates](https://core.telegram.org/bots/api#getupdates) 中的 `message.from.id` 和 `message.chat.id`. 不要把 token 交给第三方 ID 查询网站. 发送消息时使用个人身份, 不使用匿名管理员或频道身份.

最简单的方式是直接与 bot 普通私聊. 私聊不强制使用 topic; 需要在同一个 Telegram chat 中同时运行多个独立 OMP 会话或项目时, 再使用多个 topic. 群组中必须使用 topic.

### 2. 准备配置文件

将下面的默认配置保存为 `~/tool/omp-telegram/config.toml`, 放在二进制旁. 也可以复制仓库中的 [config.toml](config.toml), 再按需修改:

```toml
[telegram]
token = "${OMP_TELEGRAM_BOT_TOKEN}"
allowed_users = ["${OMP_TELEGRAM_ALLOWED_USERS}"]
allowed_chats = ["${OMP_TELEGRAM_ALLOWED_CHATS}"]
# Telegram 任务实时进度.
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

未指定配置文件, 且默认文件不存在时, 程序会使用同样的内置默认配置, 不生成文件.
`storage.database_retention_days` 启用 Database Message Retention janitor. 它按最后一次状态转换时间保留设定天数的终态 inbox/outbox 记录. 默认值是 `90`; 设置为 `0` 可关闭清理. pending/submitted inbox 及 pending/sending outbox 保持持久化. 它绝不删除 binding、history、startup intent、工作目录、omp session 文件或其他 omp 数据.

`telegram.progress_mode` 控制一条尽力而为、可编辑的任务实时消息: `off` 关闭它和 typing action, `summary` 显示 assistant 输出、活动工具名和任务状态, `verbose` 额外显示最近 6 条可观察的工具活动. 活跃根任务的实时进度带有 Stop 按钮, 只取消当前任务并发送原生 `abort`; 不同于 `/stop`, 它保留 bridge 延后 prompt, 当前任务取消完成后按原顺序继续执行. 按钮绑定其 owner 和活跃任务. 对于正常 `done` 任务, 实时进度会保留到全部持久化最终回复确认送达后再尽力删除. `cancelled` 和 `uncertain` 任务的 progress 保留. 已确认的不可重试 Telegram 删除拒绝会释放持久化 progress 关联. 当终态数据达到 database retention cutoff 且没有 pending 或 sending outbox 工作时, retention 可以只清除本地关联, 保留 Telegram progress 消息. 传输失败、429、5xx 和不确定响应会保留关联以便后续重试. message ID 只作为 bridge 交付 metadata 持久化; bridge 不复制 progress 正文, 也不影响最终回复的可靠交付. 实时进度回复对应的根用户消息. 它绝不包含 reasoning、工具参数、命令文本、结果、stdout 或 stderr.

运行中的任务会在新任务开始后的三秒初始延迟之后, 于正常 worker tick 中发送 progress 及 Stop 按钮. 已有消息更新、typing 和持久化最终回复保持独立行为. 首条 progress 请求进行期间仍可使用 `/stop`.

RPC 终结事件丢失不代表成功: 至少 30 秒没有活动后, bridge 需要两次明确的原生 idle 观测, 且间隔至少 30 秒, 才会持久化标记任务为 `uncertain`, 绝不重放该任务. 恢复会停止 typing、清理任务控件, 并先关闭旧进程, 再让队列中的任务按需恢复同一已保存 session. compact、retry、工具、host request 和原生 UI 等待会阻止恢复; 缺失状态字段或探测错误都不是 idle 证据. 此安全 watchdog 独立于 `worker.idle_timeout`.

明确收到 `agent_end isTerminal=false` 后, watchdog 不会恢复正在等待原生异步续接的任务, 即使原生状态连续几分钟显示 idle. 后续 `agent_start` 才重新允许对运行中的 turn 进行恢复; 任务终结或替换会清除等待状态. 无法安全区分续接事件丢失与后台工作仍未完成, 因此不自动恢复这种等待.

`worker.idle_timeout` 会在 OMP 运行期持续空闲时关闭其进程, 但保留已验证的 session 身份和恢复资格. 默认值为 `30m`; 设置 `0` 或 `disabled` 可关闭. bridge 不会在任务、排队或准备中的 prompt、compact、handoff、会话列表请求、host request、启动转换, 或 model、thinking、fast、compact、new、原生 UI 等 runtime-bound confirmation 仍存在时释放进程; 这些 confirmation 会让 runtime 保持活跃, 直到被消费或过期. 独立的 `/resume` 菜单不阻止释放, runtime 释放后仍可继续操作. 下一条 prompt 或需要 OMP 状态的原生控制命令会先恢复同一个 session, 再被接受. `/status` 显示保留的 binding 和不可用的实时指标; 如果当前 generation 尚未连接过 runtime, 显示 `Idle: n/a`. released 后 `/stop` 只清 bridge prompt, 不发送 `abort`; `/close` 清除保存的恢复资格, 不启动 omp.

### 3. 设置环境变量并启动

设置配置中引用的环境变量, 将示例 token 和 ID 替换为你自己的值:

```sh
export OMP_TELEGRAM_BOT_TOKEN='your-bot-token'
export OMP_TELEGRAM_ALLOWED_USERS=123456789
export OMP_TELEGRAM_ALLOWED_CHATS=123456789

~/tool/omp-telegram/omp-telegram --check
~/tool/omp-telegram/omp-telegram
```

私聊的 chat ID 与你的 user ID 相同. 群组则填写负数 chat ID, 例如 `-1001234567890`. 多个 ID 用逗号分隔, 用户和聊天必须同时命中白名单.

请保护好 token. 直接在终端输入可能留在命令历史中; 包含 token 的启动文件也应限制访问权限, 不要提交到 Git.

`--check` 检查本地配置和 omp 可执行文件, 并创建配置中的数据及工作目录, 不验证 Telegram 或模型认证. 第二条命令前台运行服务, Ctrl-C 退出.

### 4. 在私聊或 topic 中开始对话

```text
/new demo
```

默认工作目录根路径下, 这会在二进制旁的 `workspace/demo` 中开启新会话, 目录不存在时自动创建. 也可以指定运行服务的机器上的项目目录:

```text
/new /home/you/projects/my-app
```

等收到就绪消息后, 直接发送普通文字即可. 已有文件不会被复制或清空.

首次直接发送 `/new` 会使用 `storage.workspace_root` 本身, 默认是可执行文件旁的 `workspace/`. 后续沿用当前对话上次选择的目录. 不同对话使用默认目录时会话独立, 但文件共享; 需要独立项目目录时使用 `/new <项目名>`.

## 对话命令

| 命令 | 用途 |
| --- | --- |
| `/new <名称或路径>` | 在指定目录开启新会话, 替换已有实例前需要确认 |
| `/new` | 沿用历史目录开启新会话; 没有历史目录时使用 `storage.workspace_root` |
| `/stop` | 中止当前任务并清空排队消息, 保留会话; released session 只清空消息 |
| `/close` | 关闭当前逻辑 session, 保留文件和会话历史; 不唤醒 released runtime |
| `/resume` | 用分页按钮选择当前目录中的 omp 历史会话 |
| `/resume <session ID>` | 用原生 omp session 及其原目录替换当前逻辑 session |
| `/status` | 查看目录, 会话标题/ID, 模型, 思考等级, Fast, 上下文用量, 活动状态, 队列和速度 |
| `/name <名称>` | 命名当前 omp session, 例如 `/name Bugfix HAL`; 不修改 Telegram topic 名称 |
| `/model` | 用按钮选择 OMP 配置的 cycle 角色, 同时显示当前模型 |
| `/model provider/model` | 空闲时切换模型 |
| `/thinking` | 空闲时用按钮选择思考等级, 显示当前等级 |
| `/fast [on\|off\|status]` | 用按钮选择 Fast, 显式开关, 或查看设置与实际生效状态 |
| `/compact` | 空闲时经确认压缩上下文 |
| `/handoff [补充要求]` | 空闲且队列为空时执行 OMP 原生 handoff, 可附带交接要求 |
| `/review [arguments]` | 作为独立任务运行 omp 原生 `/review` 命令 |
| `/help` | 查看帮助 |

以上所有命令, 包括 `/new <名称或路径>` 和 `/resume`, 都可用于普通私聊和 topic. 普通私聊使用 `(chat, 0)`, 沿用 topic 的 worker 和会话生命周期, 不需要单独的私聊 worker 或数据库迁移. `/followup` 仍不支持.

普通文字, 附件和 `/review` 都是由 bridge 排队的独立任务, 在每个对话内串行执行. 任务运行期间发送的消息会等待当前任务结束. `/stop` 先清空 bridge 队列, 仅在 OMP 已连接时发送普通 `abort`; released runtime 没有原生任务, `/stop` 不会启动它.

模型菜单按 OMP 的 `cycleOrder` 列出角色, 例如 `smol`, `default`, `slow`, 不罗列全部可用模型. 所选角色及其 thinking 设置由 OMP 自己解析. 打开菜单不会切换模型; 点击选择时实例必须空闲且队列为空. 选择后清除按钮, 回复实际选中的模型. 仍可手动使用 `/model provider/model`.

角色菜单支持 `omp.args` 中的 `--config 路径` 和 `--config=路径`, 多个文件按原顺序应用. 原生查询先加载继承的 `PI_CONFIG_FILES`, 再加载这些覆盖文件; 相对路径以 worker 工作目录为基准. 不改写配置文件. `--profile`, `--smol`, `--slow`, `--plan` 等运行时覆盖项仍需使用明确的 `/model provider/model` 切换.

`/thinking` 提供固定等级 `off`, `minimal`, `low`, `medium`, `high`, `xhigh`, `max`. 菜单标记当前实际生效等级; OMP 可能按模型能力调整请求等级. bridge 不通过 `set_thinking_level` 发送类型契约未声明的 `auto`. OMP 已有配置和模型角色的 thinking 设置保持不变.

bot 菜单, 按钮和服务提示使用英语. 你可以用任意语言提问, 程序不会翻译模型回复.

### 重启与恢复

- 服务重启后会恢复之前仍打开的会话. 已 `/close` 的保持关闭, `/stop` 不影响恢复资格.
- 中断的任务不会自动重跑, 包括 `/review` 在内的待执行任务会取消, 不会自动重放. 重发前先检查聊天和文件, 避免重复执行.
- 会话文件或原工作目录缺失时会报错, 不会创建新会话冒充恢复. 启动或切换过程中异常中断, 可能需要手动 `/resume`.
- **删除 topic 前先发送 `/close`.** 删除 Telegram topic 本身不会自动停止对应的 omp 实例.

## 图片和文件

使用 Telegram 的附件按钮发送照片或文档, 在 caption 中说明要做什么. 没有 caption 时, 默认请 omp 查看附件. 相册按多条消息分别处理.

需要回传文件时, 可以直接说 "把报告作为文件发给我". omp 可以发送当前工作目录内的普通文件. "已加入发送队列" 不代表已经送达, 请确认聊天中实际出现了附件.

| 传输方向 | 限制 |
| --- | --- |
| 从 Telegram 下载 | 20 MB |
| 发送文档 | 50 MB |
| 发送照片 | 10 MB, JPEG 或 PNG |

收到的原件保留在工作目录的 `.telegram/incoming/` 下. 大图可能以预览或本地文件路径提供给模型. 识图能力取决于模型, 文档读取能力取决于可用工具. `/stop` 不会删除已经提交给 omp 的附件原件.

## 配置

配置按以下顺序选择:

1. 通过 `--config <路径>` 或 `-c <路径>` 指定的文件.
2. 未指定时, 读取真实二进制所在目录的 `config.toml`, 符号链接会先解析.
3. 默认文件不存在时, 使用内嵌配置, 不生成文件.

显式指定的文件不存在, 文件不可读或 TOML 格式错误时都会报错. 上面的分组默认配置已包含必填白名单.

配置现在使用分组 TOML table: `[telegram]`, `[omp]`, `[storage]`, `[worker]`, `[logging]` 和可选 `[logging.component_levels]`. 根级 flat 字段, 原 `[log_component_levels]` table, 放错 table 的字段以及 flat/grouped 混合布局都会拒绝. 这是有意的 breaking cutover: 升级前必须手动迁移现有私有配置; 程序不会自动重写.

| 配置项 | 说明 |
| --- | --- |
| `telegram.token` | Bot token, 推荐引用环境变量 |
| `telegram.allowed_users`, `telegram.allowed_chats` | 必填的数字 ID 白名单, 可直接填写整数, 或引用逗号分隔的环境变量值 |
| `telegram.progress_mode` | 任务实时进度: `off`, `summary` 或 `verbose`; 默认 `summary` |
| `omp.binary` | 从 `PATH` 查找的可执行文件名, 或绝对路径, 不是 shell 命令 |
| `omp.args` | omp 额外参数, 默认读取可选的 `OMP_TELEGRAM_ARGS`; 显式空字符串禁用额外参数 |
| `storage.data_dir` | 数据库, 锁和待发送附件的存储目录, 默认二进制所在目录 |
| `storage.workspace_root` | `/new <名称>` 使用的根目录, 默认读取可选的 `OMP_TELEGRAM_WORKSPACE_ROOT`, 再回退到二进制旁的 `workspace/` |
| `storage.database_retention_days` | 终态数据库消息保留天数, 默认 `90`, 设置为 `0` 可关闭清理 |
| `worker.max_workers` | 同时运行的 OMP 进程上限, 默认 4 |
| `worker.queue_capacity` | 每个对话的等待消息上限, 默认 16 |
| `worker.idle_timeout` | 释放持续空闲 OMP 进程前的时长, 默认 `30m`; 设置 `0` 或 `disabled` 可关闭 |
| `logging.level` | 全局结构化日志级别: `debug`, `info`, `warn` 或 `error`; 默认 `info` |
| `logging.format` | 结构化日志格式, `text` 或 `json`; 默认 `text` |
| `[logging.component_levels]` | 可选的组件级别覆盖, 组件只能是 `daemon`, `bridge`, `rpc`, `telegram`, `store` 和 `media` |

字符串支持 `$VAR` 和 `${VAR}`, `$$` 表示字面美元符号. 所有分组 table 中的字符串都在 TOML 解析后按相同规则展开, 且每个值只展开一次. 默认引用的 `OMP_TELEGRAM_ARGS` (`omp.args`) 和 `OMP_TELEGRAM_WORKSPACE_ROOT` (`storage.workspace_root`) 可以未设置; 包括 token 和必填白名单在内的其他缺失引用会报错. 程序不会自动加载 `.env` 或 shell 启动文件.

结构化日志只在 TOML 中配置, 没有专用的日志环境变量. `logging.level`, `logging.format` 以及 `[logging.component_levels]` 中的值沿用 `$VAR`, `${VAR}` 和 `$$` 规则. 组件名会先按六个支持的名称校验, 再展开覆盖值. `text` 使用紧凑的 `YYYY-MM-DD HH:MM:SS LEVEL [component] message key=value` 单行格式, 保留原始消息和其余所有结构化属性, 头部不再包含 `time=`, `level=`, `msg=` 或 `component=` 标签. 字符串和控制字符按需转义, 确保每条记录只有一行. `json` 保持标准 `slog.JSONHandler` 输出不变, 每行一个可独立解析的对象, 包含 `component` 字段. 组件表只覆盖指定组件的 `logging.level`; 未指定的组件继承全局级别.

text 示例:

```text
2026-09-19 13:20:01 WARN [telegram] telegram polling failed event=poll_failed reason=timeout
```

`--version` 和成功的 `--check` 输出保持不变. logger registry 创建前发生的配置和 CLI 错误保持普通可读文本, 即使配置了 `logging.format = "json"`. registry 创建后, daemon 日志包含锁和数据库启动失败, 都按所选格式输出到 stderr. 日志存储和轮转仍由进程管理器负责.

使用其他位置的桥接配置:

```sh
~/tool/omp-telegram/omp-telegram -c "$HOME/.config/omp-telegram/config.toml" --check
```

### 给 omp 传入参数

例如, 使用你已经准备好的 omp 配置 overlay:

```sh
export OMP_TELEGRAM_ARGS="--config \"$HOME/.config/omp/telegram.yml\""
```

这是 **omp 的配置**, 不是桥接配置. 也可以直接在 TOML 中设置 `omp.args`. 参数支持引号, 但不会经过 shell 执行; 配置文件请使用绝对路径. 桥接不会自动修改 omp 的配置, 凭据, 工具或审批策略. RPC 模式, 工作目录及会话生命周期参数由桥接管理, 不能自行覆盖.

修改配置或环境变量后需重启服务. 后台运行方式由你自行选择, 请给进程管理器明确配置环境变量, 并确保它的 `PATH` 能找到 omp 及其运行时.

## 数据, 升级与安全

默认运行数据位于二进制旁:

```text
~/tool/omp-telegram/
├── omp-telegram
├── config.toml          # Optional
├── omp-telegram.db
├── daemon.lock
└── workspace/
```

**相对 `storage.data_dir` 和 `storage.workspace_root` 均以二进制目录为基准**, 不是启动目录或配置文件所在目录. 移动二进制可能会使用另一份数据库, 数据需要独立存放时请使用绝对路径. 显式传入的相对 `--config` 路径是例外, 它相对于调用目录.

升级前先停止服务, 备份数据目录, 工作目录和 omp 自己的会话存储. 只备份桥接数据库并不等于备份完整 omp 对话. 不要删除 SQLite 的 `-wal`/`-shm` 文件, 也不要在写入期间只复制主数据库. 旧的无版本开发数据库不会自动升级; 若启动提示结构不支持, 先备份并使用新的数据目录.

用 `~/tool/omp-telegram/omp-telegram --version` 或 `-v` 查看安装版本.

- 只授权可信用户. omp 拥有服务用户的文件访问权限和环境变量, 不同对话的独立会话不是文件系统或凭据沙箱.
- 群成员可能看到提问和回复, 即使他们无权控制 bot. 数据库也会保存消息内容, 并按配置的 `storage.database_retention_days` 策略清理.
- 发送超时仍可能已经送达, 不要把没有收到回复理解为任务没有执行.
- 优先正常退出服务, 不使用 `kill -9`; 强制终止不能保证所有工具子进程都退出.

## 常见问题与限制

| 现象 | 检查项 |
| --- | --- |
| bot 没有反应 | 两个白名单, 群组是否使用 topic, 群隐私设置, 是否有其他 polling 服务或 webhook 占用 bot |
| `omp executable not found` | 服务进程的 `PATH` 是否能找到 omp 和运行时, 不要只检查交互式终端 |
| `/resume` 列表为空 | 当前选择的目录是否正确, omp 是否已经保存历史; 新的空会话可能尚无可恢复文件 |
| `/compact` 失败 | 短会话可能没有可压缩内容; 若本机 omp 也压缩失败, 检查其模型配置 |
| 数据库结构不支持 | 先备份, 使用受支持的数据库或新数据目录, 不要手动改版本号 |

不支持语音/转写, 自动创建 topic, 任意终端输入框或编辑器. 部分确认/选择交互可以使用 Telegram 按钮, 但并非所有工具审批都能远程完成. 桥接不会开启自动批准; 无人值守前, 先确认你依赖的审批流程能够正常使用.

## 路线图

以下功能均为计划项, 尚未实现.

- 列出已保存的对话/session 绑定
- 查看 bridge 队列状态, 取消单个待执行任务
- Telegram 回复上下文
- Telegram 媒体组 / 相册支持
- Session 导出
- Resume 收藏 / 置顶 session
- `/doctor` 诊断

### 等待上游 OMP 支持

- 可靠的活动 turn steering
- 原生 follow-up 支持
- 原生队列清空 / 原子 abort-and-clear

开发者请参阅[架构与开发说明](doc/architecture.zh.md).
