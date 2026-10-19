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

每个普通私聊或 topic 都有独立的 OMP session. 不同对话可以并行工作.

## 运行要求

- Linux, amd64 或 arm64.
- 已安装支持 RPC protocol v2 的 omp 及其运行时, 例如 Bun. 使用运行服务的同一用户配置模型和认证.
- Git 仅在使用 OMP 原生 autoresearch 工作流时需要.
- 能访问 Telegram 和模型服务.
- 一个 Telegram bot. 支持普通私聊, 私聊 topic 和群组 topic; 不支持没有 topic 的群组消息.

已在 OMP 18.12.2 测试通过. 验证场景与未验证项见[架构文档](doc/architecture.zh.md).

同一个 bot token 同时只能由一个 polling 服务使用. 不能与 webhook 或其他 `getUpdates` 消费者同时运行.

## 安装

### 下载发布版

从 [Releases](https://github.com/fcying/omp-telegram/releases) 下载对应架构的压缩包. amd64 示例:

```sh
mkdir -p "$HOME/tool/omp-telegram"
tar -xJf omp-telegram-linux-amd64.txz -C "$HOME/tool/omp-telegram"
```

arm64 使用 `omp-telegram-linux-arm64.txz`. 压缩包包含二进制和 LICENSE, omp 需要单独安装. 可选迁移 Extension 内嵌于二进制. 每个发布版都附带 `SHA256SUMS`.

开发构建发布在 [Development build](https://github.com/fcying/omp-telegram/releases/tag/dev-latest). 这是 prerelease, 可能不稳定.

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

### 配置文件和环境变量

配置按以下顺序选择:

1. 通过 `--config <路径>` 或 `-c <路径>` 指定的文件.
2. 解析符号链接后的真实二进制所在目录中的 `config.toml`.
3. 默认文件不存在时使用内嵌默认配置.

显式指定的文件不存在, 文件不可读或 TOML 无效时, 启动都会失败. 配置必须使用上面示例中的分组 table; 旧的 flat 字段和 flat/grouped 混合布局不再接受.

显式相对 `--config` 路径以启动目录为基准. 相对 `storage.data_dir` 和 `storage.workspace_root` 始终以真实可执行文件目录为基准, 不以配置文件目录为基准.

配置字符串支持 `$VAR` 和 `${VAR}`, 但 `[omp.environment]` 的模式与 `allow`/`deny` 变量名为字面值, 不展开引用. `$$` 表示字面美元符号. TOML 解析后其他环境变量引用只展开一次. 默认引用的 `OMP_TELEGRAM_ARGS`, `OMP_TELEGRAM_PROGRESS_MODE` 和 `OMP_TELEGRAM_WORKSPACE_ROOT` 可以未设置. `telegram.progress_mode` 为空或非法时回退到 `summary`. token 和白名单必须提供. 程序不会自动加载 `.env` 文件.
仅用于显示的 `omp.args_labels` 按字面读取, 不展开环境变量.

`telegram.allowed_users` 和 `telegram.allowed_chats` 都是必填项. 只有发送者和 chat 同时命中白名单时才接受 update. 普通私聊的 chat ID 通常等于 user ID; 群组 chat ID 通常是负数.

### 配置项

| 配置项 | 说明 |
| --- | --- |
| `telegram.token` | Telegram bot token. 推荐引用环境变量. |
| `telegram.allowed_users`, `telegram.allowed_chats` | 必填的数字 ID 白名单. 可直接填写整数, 或引用逗号分隔的环境变量值. |
| `telegram.progress_mode` | 任务实时进度: `off`, `summary` 或 `verbose`. 默认读取 `OMP_TELEGRAM_PROGRESS_MODE`; 为空或非法时回退到 `summary`. 见[进度界面](#进度界面). |
| `omp.binary` | 从 `PATH` 查找的可执行文件名或绝对路径, 不是 shell 命令. |
| `omp.args` | 按顺序排列的 OMP 启动参数预设. 默认只有一个可选的 `OMP_TELEGRAM_ARGS` 预设. 旧的字符串格式仍作为单个预设接受. |
| `omp.args_labels` | 可选的 Telegram picker 标签, 每个预设一个. 未设置、为空或数量不匹配时回退显示 `Preset N`, 并在启动时记录 warning. 过长标签会截短以适配按钮. 标签会展示给能查看对话的人, 不要填凭据. |
| `omp.migration_extension` | 为所有参数预设在每次 OMP 启动时加载内嵌迁移 Extension, 包括 resume. 默认 `false`, 无需单独安装 Extension. |
| `[omp.environment]` | 可选的子进程环境策略. 默认 `mode = "denylist"` 会传递当前及未来新增的服务环境变量, 只排除显式列入 `deny` 的名称. 随附的 `config.toml` 设置了 `deny = ["OMP_TELEGRAM_BOT_TOKEN"]`. `mode = "allowlist"` 只传递列出且已存在的变量. |
| `storage.data_dir` | 数据库, lock, 输入/输出附件及 bridge 生成的 Extension 缓存目录. 默认是二进制所在目录. |
| `storage.workspace_root` | `/new <名称>` 使用的根目录. 默认读取可选的 `OMP_TELEGRAM_WORKSPACE_ROOT`, 再回退到二进制旁的 `workspace/`. |
| `storage.database_retention_days` | Bridge 终态消息和符合清理条件的附件缓存的保留期. 默认 `90` 天; `0` 关闭按保留期执行的自动清理. 不删除 workspace, OMP session 数据或已保存的对话 binding. 见[数据与保留](#数据与保留). |
| `worker.max_workers` | 同时连接的 OMP 进程上限. 默认 `8`; 配置最大值 `64`. |
| `worker.queue_capacity` | 每个对话中等待任务与未解决 steer 的共享上限. 默认 `16`; 配置最大值 `1024`. |
| `worker.idle_timeout` | 保留 session 的同时释放持续空闲 OMP 进程前的时长. 默认 `30m`; `0` 或 `disabled` 关闭. |
| `logging.level` | 全局日志级别: `debug`, `info`, `warn` 或 `error`. 默认 `info`. |
| `logging.format` | 日志格式: `text` 或 `json`. 默认 `text`. |
| `[logging.component_levels]` | 可选的组件级别覆盖: `daemon`, `bridge`, `rpc`, `telegram`, `store` 和 `media`. |

`omp.args` 数组中的每项都是一套完整的 shell-style 参数字符串, 只解析引号分组, 直接传递且不经过 shell. 除非通过这些参数或用户请求的命令显式选择, 否则不改变 OMP 自身配置.

`/args` 的选择作用于所有对话, 并跨服务重启保存. 修改预设定义或展开后的参数值会在启动时重置为预设 1; 仅修改展示标签不会重置. 已连接的 OMP 进程保留原启动参数.

要显式限制 OMP 子进程, 用下面的内容替换已有 `[omp.environment]` table. 不要同时配置 `allow` 和 `deny`, 即使其中一个列表为空.

```toml
[omp.environment]
mode = "allowlist"
allow = ["PATH", "HOME", "YOUR_PROVIDER_API_KEY"]
```

`allow` 列出 OMP 所需的已存在环境变量, 包括 provider 凭据和 `PATH` 等运行时变量. 名单不完整可能导致 OMP 无法运行. 这只限制环境变量继承, 不限制文件系统或同用户进程访问.

若只需排除部分变量, 其余变量保持传递:

```toml
[omp.environment]
mode = "denylist"
deny = ["OMP_TELEGRAM_BOT_TOKEN", "SOME_API_KEY"]
```

`deny` 列出不传给 OMP 子进程的字面变量名; bridge 侧的 TOML `${VAR}` 仍会展开. 如果使用自定义 Telegram token 变量名且不希望 OMP 继承, 将其加入这里.

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
| `/new <名称或路径>` | 在指定目录开启新 session. 替换正在运行的 session 前需要确认. Topic 的首次 session 会尝试将标题设为 workspace 名称. |
| `/new` | 在上次目录开启新 session; 没有历史目录时使用 `storage.workspace_root`. |
| `/stop` | 中止当前任务并清空排队任务, 保留 session 供后续使用. |
| `/queue` | 查看运行及排队任务. Cancel 按钮只删除选中的等待任务, 不中止当前任务. |
| `/followup <message>` | 明确排队一个独立任务, 不会作为 steer 发送给当前任务. |
| `/close` | 关闭当前 session, 保留文件和 OMP history. |
| `/bindings` | 列出当前 chat 保存的对话/session binding. 点击可删除条目的标题, 确认后忘记已关闭或空闲的 binding; 保留 workspace 文件和 OMP history. |
| `/bindings old` | 按最久未使用优先的顺序显示已保存的 binding. |
| `/resume` | 从当前 workspace 选择已保存的原生 OMP session; 可 Pin/Unpin, 或经确认后 Delete. |
| `/resume <session ID>` | 恢复指定的原生 OMP session; OMP 可能恢复该 session 记录的工作目录. |
| `/export` | 选择已保存的 session, 导出原始 main-session JSONL. 导出当前 session 前需等待其空闲. |
| `/export html` | 选择已保存的 session, 导出 standalone HTML viewer. |
| `/export <session ID>` | 不打开 picker, 直接导出指定 session 的原始 OMP main session `.jsonl`. 使用 `/export html <session ID>` 显式选择 HTML. |
| `/status` | 查看当前 workspace, session, model, thinking, fast mode, context, runtime 和队列状态. |
| `/args [预设编号]` | 查看预设标签并切换所有对话后续新启动进程使用的 OMP 参数. 不影响已运行的 OMP 进程. |
| `/doctor` | 异步运行安全的 bridge 诊断, 不启动或修改 OMP session. |
| `/name <名称>` | 命名当前 OMP session. 不修改 Telegram topic 名称. |
| `/model` | 用按钮选择 OMP 配置的 model role. |
| `/model provider/model` | 空闲时切换到指定 model. |
| `/thinking` | 空闲时选择 thinking level. |
| `/fast [on\|off\|status]` | 选择 fast mode, 显式开启或关闭, 或查看状态. |
| `/compact` | 空闲时直接压缩当前 context, 无需确认. 状态消息原位更新结果和 context 使用量. |
| `/handoff [补充要求]` | 空闲且队列为空时运行 OMP 原生 handoff, deadline 为 180 秒. |
| `/migrate` | 在同一 workspace 中将 handoff 上下文迁入新 session. 先启用 `omp.migration_extension`. |
| `/review [arguments]` | 作为排队任务运行 OMP 原生 `/review` 命令. |
| `/autoresearch [goal\|off\|clear]` | 当前 OMP 公布支持时控制原生 autoresearch. 安全边界和停止行为见下文. |
| `/help` | 查看帮助和当前 bridge 版本 (与 `-v` 一致). |

Model 或 thinking 修改成功后, 仍可能因无法读取配置等级而显示 `Thinking: n/a`. 这不会撤销修改; 如果必须关闭进程, 回执会说明 session 已保留.

对话空闲时, 普通文字会启动任务. OMP 任务运行期间, OMP 可能接受普通文字作为 steer. 使用 `/followup <message>` 可明确排队一个独立任务. 附件、`/review` 和其他受支持的原生命令也进入对话队列并按顺序执行. 不同对话可以并行工作, 受 worker 配置上限影响.

### OMP 原生命令

Bridge 命令优先于 OMP 命令; 发给其他 bot 的命令会被忽略. Bridge 和 builtin 参数可用空格或 `:` 分隔, 例如 `/handoff:focus` 和 `/fast:on`. `/compact` 不接受参数.

原生命令是否可用取决于 OMP 和当前 session. Bridge 会拒绝无法安全同步的生命周期变更命令; 常见的不支持命令见下表.

对原生 `/autoresearch` extension 提供专项支持. 以下原生命令尚未接入:

| 命令或类别 | 限制与替代入口 |
| --- | --- |
| `/move` | 拒绝执行: 原生 workspace/session 移动不会与 bridge binding 对齐. 请用 `/new` 或 `/resume`. |
| `/wt`, `/worktree` | 拒绝执行: bridge 不管理原生 worktree 创建和切换. 先在 OMP 或终端准备 worktree, 再用 `/new` 或 `/resume` 选择. |
| `/session delete` | 拒绝执行: 请用 `/resume` 的 **Delete**, 保留确认和 in-use 检查. |
| `/plan`, `/plan-review` | 依赖上游 OMP RPC 支持. Bridge 不实现其 planning workflow; RPC 不支持时请使用交互式 OMP. |
| 其他 extension 及 extension alias | 拒绝执行, 包括 autoresearch 的 alias: 未接入通用 extension session 生命周期变化. 请直接在 OMP 中使用. |
| 已公布但无法识别来源的命令 | 拒绝执行, 不猜测安全性, 也不当作普通模型文字转发. |

Telegram 菜单列出 bridge 命令及当前可用的原生命令. Telegram 每份菜单最多 100 项, 且命令名字符受限; 未显示但受支持的命令仍可手动输入.

启动时使用一个临时 OMP RPC 进程, 先发现内建命令再发布 chat 菜单. 不发送 prompt, 不保存 session; workspace 专属命令在实际会话连接后补充. 发现失败时仍提供 bridge 菜单.

原生命令及别名使用 OMP 公布的简介; 没有可用简介时保留通用说明. 这些简介对所有能查看该 chat 的成员可见.

原生命令的文字输出, 包括 `/shake` 摘要, 会作为独立消息发送. 本地命令不需要模型回复; 后续后台输出不代表任务已经完成.

命令识别和来源限制见[架构文档](doc/architecture.zh.md#原生命令识别与提交).

以上命令都可用于普通私聊和 topic. bot 菜单, 按钮和服务提示使用英语; 可以用任意语言提问, bridge 不翻译模型回复.

在群组 topic 中, 所有获准使用 bot 的用户共享该 topic 的 OMP session 和 workspace, 包括上下文及命令效果. bot 的用户白名单不限制 Telegram 群成员查看消息: 能查看该 topic 的其他成员也能看到 bot 回复和导出文件. 敏感任务只应在可信的群组 topic 中运行.

### Autoresearch

要求 OMP 公布原生 `autoresearch` extension. 请直接发送 `/autoresearch` 命令; 附件 caption 不会调用该命令.

- `/autoresearch <goal>` 开启研究模式并启动原生目标.
- `/autoresearch` 切换模式. 开启新实验时只确认模式已启用; 下一条普通消息提供目标.
- `/autoresearch off` 关闭研究并中断活动研究任务; 等待中的 bridge 任务保留. 使用 `/stop` 清空队列.
- `/autoresearch clear [--keep-tree] [--reset-tree]` 要求实例空闲, 队列为空并经确认. OMP 会删除研究 artifacts, 还可能重置 tracked 文件或删除 untracked 文件. `--keep-tree` 跳过工作树 reset/clean, 但仍会删除研究产物; 它不是备份或无破坏模式. 只在可接受这些改动时使用.

两步用法:

```text
/autoresearch
优化当前工程的启动时间, 保持对外行为不变.
```

研究循环由原生 OMP 管理, 参见[原生模式说明](https://github.com/can1357/oh-my-pi/blob/v18.10.3/packages/coding-agent/src/autoresearch/prompt-setup.md). 它可能创建分支、修改文件、提交保留的实验、回滚丢弃的实验并多次调用模型. 请使用合适的 workspace, 并考虑费用和文件变更.

Bridge 不会在不同对话之间锁定或隔离 workspace. 使用同一 workspace 的 OMP session 可能修改相同文件; 请使用不同 workspace, 或避免同时运行会修改文件的任务. 服务重启后不会自动重放研究任务; 决定是否重发不确定的任务前, 检查 `/status` 和 workspace.

### 删除已保存的 session

在 `/resume` 中点击 **Delete**, 再用红色 **Delete** 按钮确认. Bridge 只会在确认没有活动 runtime 和进行中操作后显示删除确认. 如果仍有工作, 会说明阻止原因, 不提供强制删除选项. OMP 会删除选中的 session history 和 artifacts, bridge 会清理引用它的已保存 binding 和 pin. Workspace 文件和 Telegram topic 保留. 删除不可撤销.

### Session 导出

`/export` 会列出当前 workspace 中保存的 OMP session, 并将选中的原生 `.jsonl` session 文件发送到 Telegram. 默认格式是 OMP 原始 main-session JSONL. `/export html` 则将选中的 session 导出为 standalone HTML viewer.

两种格式都只导出 main session, 不包含 companion 或 subagent transcript, 并受 50 MB document 限制.

也支持直接指定 session:

```text
/export <session-id>
/export html <session-id>
```

要在另一台电脑上使用原生 JSONL 导出, 准备对应的源码目录, 下载文件, 然后在目标项目目录执行:

```sh
omp --resume /path/to/session.jsonl
```

如果 session 中记录的旧工作目录已经不存在, OMP 可能要求将 session re-root 到当前目录. 导出不是项目备份: 不额外收集 workspace 文件, Git 状态, OMP 配置, 凭据文件或环境变量. Transcript 中已记录的此类内容仍会被导出. 项目文件需要单独通过 `git clone`, `git pull`, `scp`, `rsync` 或其他方式同步.

导出的 session 文件可能包含敏感的 conversation, tool, command 和 path 数据, 包括 prompts, assistant responses, tool calls, tool results, 本地路径, 命令输出, 源码片段以及意外出现在 transcript 中的 secrets. 只应将这些文件发送到可信的 Telegram 对话. 不会额外要求二次确认; 用户输入 `/export` 本身就是明确确认.

在群组 topic 中, `/export` 将 session 文件发送到该 topic, 而不是私聊. 能访问该 topic 的其他群成员即使没有操作 bot 的权限, 也可能下载该文件.

### 常见问题与限制

| 现象 | 检查项 |
| --- | --- |
| bot 没有反应 | 两个白名单, 群组 topic, 群隐私设置, 以及是否有其他 polling 服务或 webhook 占用 bot. |
| `omp.binary executable not found` | 配置的可执行文件和服务进程的 `PATH`, 包括 omp 及其运行时. 它可能不同于交互式 shell. |
| `/resume` 列表为空 | 当前选择的目录以及 OMP 是否已经保存历史. 新 session 可能还没有可恢复文件. |
| `/compact` 失败 | 短 session 可能没有可压缩内容. 如果本机 OMP 也失败, 检查其 model 配置. |
| 数据库结构不支持 | 备份数据, 使用受支持的数据库或新数据目录; 不要手动修改版本号. |

不支持语音/转写, 自动创建 topic, 以及任意终端或编辑器对话框. 部分确认和选择流程可以使用 Telegram 按钮, 但不是所有交互式工具审批都能远程完成. bridge 从不自动批准工具操作.

过大的回复可能截断, 并明确提示. Bridge 不会截断 OMP 原生 session 历史.

## 进度界面

`telegram.progress_mode` 控制任务实时进度:

- `off` - 关闭 progress 消息和 typing action.
- `summary` - 显示 assistant 输出、活动工具和任务状态.
- `verbose` - 额外显示近期可观察的工具活动. 工具详情不脱敏, 可能含有凭据或私人内容, 能查看对话的人都可见.

新任务的 progress 会延迟约三秒, 因此短任务通常只发送最终回复. 活跃任务带有 **Stop** 按钮, 只中止当前任务; `/stop` 还会清除等待中的任务.

Progress 属于 best-effort 状态提示, 不一定显示每次任务状态变化, 也不影响任务执行或最终回复.

最终文字回复可能附带简短的 model 和 context 状态信息. 实时进度和回复头中的 thinking 图标, 以及 `/status` 的 `Thinking` 字段, 都表示配置等级, 不随每轮生效等级变化. 配置为 `auto` 时, `/status` 显示 `auto`, 精简状态头显示 `⟳`.

## 附件

使用 Telegram 附件按钮发送照片或文档, 在 caption 中说明要让 OMP 做什么; 没有 caption 时默认请 OMP 查看附件. Photo 或 document 相册作为一个任务处理, 最多 10 个文件, 使用第一个非空 caption.

需要回传文件时, 可以直接告诉 OMP, 例如: "把报告作为文件发给我". OMP 可以发送当前 workspace 中的普通文件. "附件已加入队列" 不代表已经送达, 请检查聊天中是否出现实际文件.

| 传输方向 | 限制 |
| --- | --- |
| 从 Telegram 下载 | 20 MB |
| 发送文档 | 50 MB |
| 发送照片 | 10 MB, JPEG 或 PNG |

输入附件保存到 `storage.data_dir/attachments/inbox/`, 并受配置的 retention 策略影响. 配置路径重叠时, 该目录也可能位于 workspace 内. 需要长期保留的文件请复制到附件缓存目录之外. 大图可能以预览或本地文件路径提供; 识图能力取决于模型, 文档读取能力取决于工具.

## Telegram 回复上下文

回复 Telegram 消息后发送普通文字, 附件或 `/review`, 会带入一层引用上下文. 选中的 quote 文字优先; 过长引用可能截断, 但不截断当前消息.

被回复的照片和文档只提供描述, 不会重新下载. 不展开多层 reply chain, 最终回复仍定位到当前消息.

## 会话与恢复

服务重启后, 已保存的 OMP session 仍可使用. 重启后的第一条 prompt 或 OMP 命令可能需要更长时间重连 session.

中断或结果不确定的工作不会自动重放. 停机或崩溃时, 等待中的任务会取消; 决定重发前先检查聊天和 workspace.

`/stop` 结束活动任务但保留 session. `/close` 关闭 session, 保留 workspace 文件和 OMP history. 如果 `/new` 或 `/resume` 中断, 重试前先使用 `/close`.

每个对话有独立 session, 但使用同一 workspace 的 session 会共享文件. 不同对话不是文件系统或凭据沙箱.

### 迁移大型 session

`/migrate` 先生成 handoff, 不增加模型回合地初始化一个小型新 session, 验证上下文和设置后再切换当前对话. 旧 session 和 workspace 保留. Handoff 本身使用模型, 最多等待 180 秒. 切换前必须确认 model, configured thinking (包括 `auto`) 和 fast-mode 设置一致. 所选模型写入原生 session 历史, 进程重启后恢复也保留该选择.

短会话也支持迁移, 无需修改 OMP 的保留配置.

在现有 `[omp]` 表中启用内嵌迁移 Extension, 然后重启 bridge:

```toml
[omp]
migration_extension = true
```

默认值为 `false`. 开启后所有 OMP 参数预设都会加载内嵌 Extension, 不需要另装 `.ts` 文件. 现有 `omp.args` 保持不变; 如果之前已手动为该迁移 Extension 添加 `-e`, 请移除该项, 避免加载两份. 迁移要求 session 空闲且队列为空. 其他 Extension 命令和别名仍然拒绝执行.

迁移失败, 超时或中断后, 普通输入保持锁定, 不排队也不重放. `/status` 显示操作及已知候选. `/stop` 和 `/close` 停止 runtime, 但不清除迁移锁. 显式使用 `/migrate recover old` 恢复源会话, 或 `/migrate recover <完整-session-id>` / `/migrate recover <绝对-session-文件路径>` 验证候选. 恢复不会再次执行 handoff 或创建 session, 也不会猜测最新文件作为未知候选.

`/status` 没有候选信息表示身份未知, 不表示没有创建 session. 普通日志无法补全未记录的候选. 如果无法独立取得完整 ID 或绝对文件路径, 使用 `/migrate recover old`.

迁移的是摘要文本, 不是活动工具, eval 状态或全部 session 专属 artifact. Bridge 只读取有界的本次新增 handoff 记录; OMP 恢复源 session 时仍可能需要加载旧历史.

## 数据与保留

Bridge 数据保存在 `<storage.data_dir>/omp-telegram.db`, 包括已接受的 Telegram 输入内容, 待发送/已发送回复和消息 metadata. 附件以文件形式保存在同一数据目录下. 数据库, 附件缓存及其备份都应按敏感数据保护. `storage.data_dir` 默认是解析符号链接后的真实二进制所在目录; 相对 storage 路径使用该目录, 不使用启动目录.

`storage.database_retention_days` 控制 bridge 终态消息和符合清理条件的附件缓存保留时间. 默认 `90` 天; 设为 `0` 可关闭按保留期执行的自动清理. 不删除 workspace 文件, OMP session 数据或已保存的对话 binding. 保留清理不会删除已送达的 Telegram 回复.

升级前先停止服务, 备份数据目录、workspace 及 OMP 自己的 session 存储. 只备份 bridge 数据库不等于完整备份 OMP 对话.

只授权可信用户. 服务使用其 OS 用户的文件系统权限和环境运行; 群成员即使无权控制 bot, 也可能看到 prompt 和回复. 请保护 bot token.

## 日志

日志支持 `text` 和 `json`, 由 `logging.format` 选择. `logging.level` 设置全局级别; `[logging.component_levels]` 可分别覆盖 `daemon`, `bridge`, `rpc`, `telegram`, `store` 或 `media`. 见完整[配置示例](#配置).

日志输出到 stderr, 保存及轮转由进程管理器负责. 不记录 prompt, 模型回复, 凭据, 工具负载或 workspace/session 路径. Chat 和 thread ID 仍可能识别具体对话, 应限制日志访问权限.

## 开发

```sh
just build
just fmt
just test
just check
just install
just deploy
```

- `just build` 构建二进制.
- `just fmt` 格式化全部 Go 源码和测试文件.
- `just test` 运行单元测试, 不启动或重启服务.
- `just check` 运行测试, race 检查和 `go vet`, 不启动 Telegram polling.
- `just install` 只安装二进制, 配置和数据由用户管理.
- `just deploy` 安装二进制, 然后安排五秒后在后台重启已有的 `supervisord` 服务. Recipe 返回不代表服务已经成功重启.

## 架构

实现及维护细节统一放在[中文架构说明](doc/architecture.zh.md)及[英文版](doc/architecture.md): 命令路由, RPC 完成语义, 持久化, 失败语义, 重启恢复, retention 和日志契约.
