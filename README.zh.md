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
- `PATH` 中可执行的 Git. 非 Git 目录的物理 workspace 准入检查也需要它.
- 能访问 Telegram 和模型服务.
- 一个 Telegram bot. 支持普通私聊, 私聊 topic 和群组 topic; 不支持没有 topic 的群组消息.

文字 steer 要求 OMP >= 18.3.2.

同一个 bot token 同时只能由一个 polling 服务使用. 不能与 webhook 或其他 `getUpdates` 消费者同时运行.

## 安装

### 下载发布版

从 [Releases](https://github.com/fcying/omp-telegram/releases) 下载对应架构的压缩包. amd64 示例:

```sh
mkdir -p "$HOME/tool/omp-telegram"
tar -xJf omp-telegram-linux-amd64.txz -C "$HOME/tool/omp-telegram"
```

arm64 使用 `omp-telegram-linux-arm64.txz`. 压缩包包含二进制和 LICENSE, omp 需要单独安装. 每个发布版都附带 `SHA256SUMS`.

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

### 配置文件和环境变量

配置按以下顺序选择:

1. 通过 `--config <路径>` 或 `-c <路径>` 指定的文件.
2. 解析符号链接后的真实二进制所在目录中的 `config.toml`.
3. 默认文件不存在时使用内嵌默认配置.

显式指定的文件不存在, 文件不可读或 TOML 无效时, 启动都会失败. 配置必须使用上面示例中的分组 table; 旧的 flat 字段和 flat/grouped 混合布局不再接受.

配置字符串支持 `$VAR` 和 `${VAR}`, 但 `[omp.environment]` 的模式与 `allow`/`deny` 变量名为字面值, 不展开引用. `$$` 表示字面美元符号. TOML 解析后其他环境变量引用只展开一次. 默认引用的 `OMP_TELEGRAM_ARGS`, `OMP_TELEGRAM_PROGRESS_MODE` 和 `OMP_TELEGRAM_WORKSPACE_ROOT` 可以未设置. `telegram.progress_mode` 为空或非法时回退到 `summary`. token 和白名单必须提供. 程序不会自动加载 `.env` 文件.

`telegram.allowed_users` 和 `telegram.allowed_chats` 都是必填项. 只有发送者和 chat 同时命中白名单时才接受 update. 普通私聊的 chat ID 通常等于 user ID; 群组 chat ID 通常是负数.

### 配置项

| 配置项 | 说明 |
| --- | --- |
| `telegram.token` | Telegram bot token. 推荐引用环境变量. |
| `telegram.allowed_users`, `telegram.allowed_chats` | 必填的数字 ID 白名单. 可直接填写整数, 或引用逗号分隔的环境变量值. |
| `telegram.progress_mode` | 任务实时进度: `off`, `summary` 或 `verbose`. 默认读取 `OMP_TELEGRAM_PROGRESS_MODE`; 为空或非法时回退到 `summary`. 见 [Progress UI](#progress-ui). |
| `omp.binary` | 从 `PATH` 查找的可执行文件名或绝对路径, 不是 shell 命令. |
| `omp.args` | OMP 额外参数. 默认读取可选的 `OMP_TELEGRAM_ARGS`; 显式空字符串禁用额外参数. |
| `[omp.environment]` | 可选的子进程环境策略. 默认 `mode = "denylist"` 会传递当前及未来新增的服务环境变量, 只排除显式列入 `deny` 的名称. 随附的 `config.toml` 设置了 `deny = ["OMP_TELEGRAM_BOT_TOKEN"]`. `mode = "allowlist"` 只传递列出且已存在的变量. |
| `storage.data_dir` | 数据库, lock 和输入/输出附件存储目录. 默认是二进制所在目录. |
| `storage.workspace_root` | `/new <名称>` 使用的根目录. 默认读取可选的 `OMP_TELEGRAM_WORKSPACE_ROOT`, 再回退到二进制旁的 `workspace/`. |
| `storage.database_retention_days` | 终态 bridge 记录, 未完成 session 启动及符合条件的输入附件的保留期. 默认 `90`; `0` 关闭自动 retention 清理. 见[数据与保留](#数据与保留). |
| `worker.max_workers` | 同时连接的 OMP 进程上限. 默认 `8`; 配置最大值 `64`. |
| `worker.queue_capacity` | 每个对话最多等待的任务数. 默认 `16`; 配置最大值 `1024`. |
| `worker.idle_timeout` | 保留 session 的同时释放持续空闲 OMP 进程前的时长. 默认 `30m`; `0` 或 `disabled` 关闭. |
| `logging.level` | 全局日志级别: `debug`, `info`, `warn` 或 `error`. 默认 `info`. |
| `logging.format` | 日志格式: `text` 或 `json`. 默认 `text`. |
| `[logging.component_levels]` | 可选的组件级别覆盖: `daemon`, `bridge`, `rpc`, `telegram`, `store` 和 `media`. |

`omp.args` 支持用于分组参数的 shell-style quoting, 但参数直接传递, 绝不由 shell 执行. OMP overlay 仍然属于 OMP 配置; bridge 不会自动修改模型, 凭据, 工具或审批策略.

要显式限制 OMP 子进程, 将该 table 配置为:

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
| `/new <名称或路径>` | 在指定目录开启新 session. 替换正在运行的 session 前需要确认. Topic 的首次 session 会将标题设为 workspace 名称. |
| `/new` | 在上次目录开启新 session; 没有历史目录时使用 `storage.workspace_root`. |
| `/stop` | 中止当前任务并清空排队任务, 保留 session 供后续使用. |
| `/followup <message>` | 在当前任务后排入独立的文字 prompt; 空闲时作为普通任务运行. |
| `/queue` | 查看运行及排队任务. Cancel 按钮只删除选中的等待任务, 不中止当前任务. |
| `/close` | 关闭当前 session, 保留文件和 OMP history. |
| `/bindings` | 列出当前 chat 保存的对话/session binding. 点击可删除条目的标题, 确认后忘记已关闭或空闲的 binding; 保留 workspace 文件和 OMP history. |
| `/bindings old` | 按最久未使用优先的顺序显示已保存的 binding. |
| `/resume` | 从当前 workspace 的已保存原生 OMP session 中选择恢复对象. 提供 Pin/Unpin 和 Delete 控件. |
| `/resume <session ID>` | 恢复原生 OMP session 及其原目录. |
| `/export` | 选择已保存的 session, 导出原始 main-session JSONL. 导出当前 session 前需等待其空闲. |
| `/export html` | 选择已保存的 session, 导出 standalone HTML viewer. |
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
| `/autoresearch [goal\|off\|clear]` | 当前 OMP 公布支持时控制原生 autoresearch. 安全边界和停止行为见下文. |
| `/help` | 查看帮助和当前 bridge 版本 (与 `-v` 一致). |

空闲时普通文字开启任务; 任务运行中, 普通文字 steer 当前任务, 不产生独立的最终回复. 需要独立后续任务时使用 `/followup <message>`. 附件, `/review` 和其他受支持的原生命令通过对话队列执行. 不同对话可以并行工作, 受 worker 配置上限影响.

### OMP 原生命令

Bridge 命令优先于 OMP 命令; 发给其他 bot 的命令会被忽略. Bridge 和 builtin 参数可用空格或 `:` 分隔, 例如 `/handoff:focus` 和 `/fast:on`. `/compact` 不接受参数.

其他原生命令取决于当前 OMP session 公布的支持情况. 排队命令变得不可用时会被取消, 不改成模型文字发送. 命令发现成功后, 未识别的斜杠输入 (包括文件路径及拼写错误) 按普通文字处理. 部分本地命令不会产生 Telegram 回复; `/plan` 和 `/plan-review` 仍需上游 OMP RPC 支持.

如果 OMP 不支持命令发现, 未被 bridge 接管的斜杠输入会被取消, 不转发给 OMP. 普通文字和 bridge 控制命令仍可使用. 如果命令发现暂时不可用, 等待分类的斜杠输入保留队列位置; 使用 `/queue` 和该项的 **Cancel** 可让后续任务继续.

对原生 `/autoresearch` extension 提供专项支持. 以下原生命令尚未接入:

| 命令或类别 | 限制与替代入口 |
| --- | --- |
| `/move` | 拒绝执行: 原生 workspace/session 移动不会与 bridge binding 对齐. 请用 `/new` 或 `/resume`. |
| `/wt`, `/worktree` | 拒绝执行: bridge 不管理原生 worktree 创建和切换. 先在 OMP 或终端准备 worktree, 再用 `/new` 或 `/resume` 选择. |
| `/session delete` | 拒绝执行: 请用 `/resume` 的 **Delete**, 保留确认和 in-use 检查. |
| `/plan`, `/plan-review` | 依赖上游 OMP RPC 支持. Bridge 不实现其 planning workflow; RPC 不支持时请使用交互式 OMP. |
| 其他 extension 及 extension alias | 拒绝执行, 包括 autoresearch 的 alias: 未接入通用 extension session 生命周期变化. 请直接在 OMP 中使用. |
| 无法识别来源的命令 | 拒绝执行, 不猜测安全性, 也不当作普通模型文字转发. |

Telegram 命令菜单保留 bridge 命令在前, 再加入已发现且可执行的原生命令及 alias. 同一 chat 的所有 topic 共用菜单, 执行时仍校验当前 session. Telegram 每份菜单最多 100 项, 名称只能包含 1-32 位小写英文字母, 数字或下划线. 因这些限制未进入菜单的受支持命令仍可手动输入.

Runtime 发现命令后才会显示原生命令菜单项. Discovery 完成前, 或同一 chat 中贡献菜单的 session 全部显式关闭后, 菜单只包含 bridge 命令. 菜单发布是 best-effort; 仅菜单缺项不代表受支持的命令无法直接输入.

命令解析, 来源校验及完成语义见[架构文档](doc/architecture.zh.md#原生命令识别与提交).

以上命令都可用于普通私聊和 topic. bot 菜单, 按钮和服务提示使用英语; 可以用任意语言提问, bridge 不翻译模型回复.

在群组 topic 中, 所有获准使用 bot 的用户共享该 topic 的 OMP session 和 workspace, 包括上下文及命令效果. bot 的用户白名单不限制 Telegram 群成员查看消息: 能查看该 topic 的其他成员也能看到 bot 回复和导出文件. 敏感任务只应在可信的群组 topic 中运行.

### Autoresearch

要求 OMP 公布原生 `autoresearch` extension. 请直接发送命令, 不要通过 `/followup` 或附件调用:

- `/autoresearch <goal>` 开启研究模式并启动原生目标.
- `/autoresearch` 切换模式. 没有已有实验时, 开启模式可能只提示输入下一条 prompt; 该 prompt 将成为研究任务.
- `/autoresearch off` 关闭模式并中断活动研究, 保留等待中的 bridge 任务. 模式已关闭时, 不打断正在运行的普通任务.
- `/autoresearch clear [--keep-tree] [--reset-tree]` 要求实例空闲, 队列为空, 并经确认. 原生 clear 会删除研究 artifacts, 还可能重置 tracked 文件并删除 untracked 文件. `--keep-tree` 只跳过 worktree reset, 不阻止删除研究 artifacts.

启动研究不增加二次确认. 原生 OMP 可能创建并切换 Git 分支, 修改文件, 自动提交保留的实验, 回滚丢弃的实验, 并持续调用模型. 请使用适合实验的 workspace, 并注意持续费用.

研究要求独占物理 Git worktree, 非 Git workspace 则独占该目录. 已绑定到同一位置的其他 topic 会阻止开启研究或 clear; 独占期间, 其他 topic 不能在该位置启动或恢复 session. 独立的 linked worktree 不冲突. 未确认关闭的占用会跨 idle release, `/close` 和服务重启保留: 需在原对话中恢复其所属 OMP session, 再确认 `/autoresearch off` 才能释放. 删除所属 topic 前必须先释放占用; `/close` 不会释放它, 持有占用期间不能忘记其 binding. 这只保护 bridge 管理的 session, 不隔离外部程序或工具对其他目录的任意访问.

排队研究目标遇到 workspace 冲突时保持暂停, 不提交原生命令. 重试前可用 `/queue` 取消不再需要的目标.

目录或 Git 拓扑变化导致已有占用重叠时, 其他对话仍可使用, 但冲突 workspace 中的普通工作会被拒绝. 在每个所属对话分别使用 `/autoresearch off` 或 `/stop` 确认关闭. 如果 owner 的原目录或 session 文件不可用, 需先恢复; 确认关闭前保留占用.

每轮完成的结果都会持久化交付, 但仅一轮结束不代表研究任务完成, 也不会释放等待任务. 原生 OMP 自然关闭模式时, 例如达到实验上限, bridge 会确认没有原生工作残留, 完成研究任务并继续等待任务. 普通文字 steer 研究; `/followup`, 附件和其他原生命令继续排队. Progress Stop 和 `/autoresearch off` 保留该队列; `/stop` 清空队列. 活动研究会被强制中断并标记为 uncertain, 确认原生模式关闭后才执行保留的工作. 无法确认关闭时, runtime 会关闭, 队列保持暂停. 中止不能撤销文件变更或其他工具副作用.

重启后不会自动重放研究任务. Bridge 读取 OMP session 保存的 control mode; OMP 18.8.2 的 RPC 尚不提供权威的 effective research mode. 保存的 on 可能与因 Git 分支而关闭的原生 runtime 不一致. 切换 Git 分支或发送无关工作前, 请显式关闭研究. `/status` 区分活动研究和没有活动研究任务的已保存开启模式, 但无法消除这个上游限制.

### 删除已保存的 session

在 `/resume` 中点击 **Delete**, 再用红色 **Delete** 按钮确认. 只能删除未使用的 session; 请先关闭活动 session, 或等待导出及其他删除操作完成. OMP 会删除选中的 session history 和 artifacts, 然后刷新列表. 不会删除 workspace 文件、Telegram binding 或 topic. 删除不可撤销.

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

如果 session 中记录的旧工作目录已经不存在, OMP 可能要求将 session re-root 到当前目录. Session 导出不包含 workspace 源码文件, 源码树, Git 状态, 未提交文件, credentials, OMP 配置或 shell environment. 项目文件需要单独通过 `git clone`, `git pull`, `scp`, `rsync` 或其他方式同步.

导出的 session 文件可能包含敏感的 conversation, tool, command 和 path 数据, 包括 prompts, assistant responses, tool calls, tool results, 本地路径, 命令输出, 源码片段以及意外出现在 transcript 中的 secrets. 只应将这些文件发送到可信的 Telegram 对话. 不会额外要求二次确认; 用户输入 `/export` 本身就是明确确认.

在群组 topic 中, `/export` 将 session 文件发送到该 topic, 而不是私聊. 能访问该 topic 的其他群成员即使没有操作 bot 的权限, 也可能下载该文件.

### 常见问题与限制

| 现象 | 检查项 |
| --- | --- |
| bot 没有反应 | 两个白名单, 群组 topic, 群隐私设置, 以及是否有其他 polling 服务或 webhook 占用 bot. |
| `omp executable not found` | 服务进程的 `PATH`, 包括 omp 及其运行时. 它可能不同于交互式 shell. |
| `/resume` 列表为空 | 当前选择的目录以及 OMP 是否已经保存历史. 新 session 可能还没有可恢复文件. |
| `/compact` 失败 | 短 session 可能没有可压缩内容. 如果本机 OMP 也失败, 检查其 model 配置. |
| 数据库结构不支持 | 备份数据, 使用受支持的数据库或新数据目录; 不要手动修改版本号. |

不支持语音/转写, 自动创建 topic, 以及任意终端或编辑器对话框. 部分确认和选择流程可以使用 Telegram 按钮, 但不是所有交互式工具审批都能远程完成. bridge 从不自动批准工具操作.

过大的回复可能截断, 并明确提示. Bridge 不会截断 OMP 原生 session 历史.

## Progress UI

`telegram.progress_mode` 控制任务实时进度.

支持的值:

- `off` - 关闭 progress 消息和 typing action.
- `summary` - 显示 assistant 输出、活动工具名和任务状态.
- `verbose` - 额外显示近期工具参数, 更新及结果.

新任务的 progress 会延迟约三秒, 因此短任务通常只发送最终回复. 活跃任务带有 **Stop** 按钮. 它只中止当前任务; `/stop` 还会清除对话中已经排队的任务, 按钮则让这些任务在取消完成后继续执行.

实时进度和最终回复相互独立. Progress 属于 best-effort UI, 不影响最终回复的持久化交付.

中止不能撤销工具已经产生的副作用. 如果中止需要强制终止 OMP, 执行状态可能不确定, session 也可能无法恢复. 有待处理 steer 的任务长时间空闲停滞时也可能触发恢复; 不确定的任务绝不自动重放.

**隐私风险:** `verbose` 不对工具参数和结果脱敏, 可能暴露文件路径、命令、源码、进程输出、凭据或私人文件; 群组 topic 中的其他读者也能看到. 工具详情只截断、不净化; 任务完成后删除进度消息无法撤回已经泄露的内容. `summary` 不显示工具负载, `off` 则关闭实时进度. 模型 reasoning 和整条 RPC frame 不会显示.

## 附件

使用 Telegram 附件按钮发送照片或文档, 在 caption 中说明要让 OMP 做什么; 没有 caption 时默认请 OMP 查看附件. Photo 或 document 相册作为一个任务处理, 最多 10 个文件, 使用第一个非空 caption.

需要回传文件时, 可以直接告诉 OMP, 例如: "把报告作为文件发给我". OMP 可以发送当前 workspace 中的普通文件. "附件已加入队列" 不代表已经送达, 请检查聊天中是否出现实际文件.

| 传输方向 | 限制 |
| --- | --- |
| 从 Telegram 下载 | 20 MB |
| 发送文档 | 50 MB |
| 发送照片 | 10 MB, JPEG 或 PNG |

输入文件私有保存在 `<storage.data_dir>/attachments/inbox/`, 受 retention 清理策略影响, 即使该存储目录位于 workspace 内也一样. 需要长期保留的文件请复制到受管理的附件目录之外. 大图可能以预览或本地文件路径提供; 识图能力取决于模型, 文档读取能力取决于工具. 中止任务不会删除已经提交给 OMP 的附件.

## Telegram reply context

回复 Telegram 消息后发送普通文字, 附件或 `/review`, 会带入一层引用上下文. 选中的 quote 文字优先; 过长引用可能截断, 但不截断当前消息.

被回复的照片和文档只提供描述, 不会重新下载. 不展开多层 reply chain, 最终回复仍定位到当前消息.

## Sessions and Recovery

已保存的 session 会在服务重启后保留. 第一条 prompt 或 OMP 控制命令会重连原 session, 因而可能稍慢.

结果不确定的活动操作不会自动重放.

如果 `/new` 或 `/resume` 中断, 再次尝试前用 `/close` 清除未完成的启动. Retention 也可能在超期后自动清除它, 但不删除 workspace 文件或 OMP history. 结果不确定时先检查 OMP history.

等待中的任务会在停机或崩溃后取消, 不会恢复. 决定重发前先检查聊天和 workspace.

- `/close` 后 session 保持关闭; `/stop` 不会关闭 session, 后续仍可发送 prompt.
- 已保存的 session 文件或 workspace 不可用时, bridge 会报告失败, 不创建替代 session. 请执行 `/close`, 再按需使用 `/new` 或 `/resume`.
- `worker.idle_timeout` 可以释放空闲 OMP 进程, 保留 session 供后续使用. 下一条 prompt 或 OMP 控制命令会恢复同一个 session; 新 session 在 OMP 保存 history 前可能保持连接.
- **删除 Telegram topic 前, 如果它持有研究占用, 先在该 topic 中确认 `/autoresearch off`, 再发送 `/close`.** `/close` 不会释放研究占用, 其他对话也不能接管其 owner 身份. 如果 topic 已经删除, 同一 chat 其他对话中的 `/bindings` 只能关闭并忘记没有研究占用的空闲 binding, 不能释放或转移研究占用.

每个对话有独立的 session, 但使用同一 workspace 的 session 会共享文件. 不同对话不是文件系统或凭据沙箱.
要忘记其他对话中已关闭或空闲的 binding, 打开 `/bindings`, 点击其标题并确认删除. 当前对话, 未完成启动及持有研究占用的条目不能在这里删除. 此操作只删除 bridge 元数据, 不删除 workspace 文件或 OMP 原生 history, 也不取消已排队的最终回复或附件交付.

## 数据与保留

Bridge 状态保存在:

`<storage.data_dir>/omp-telegram.db`

`storage.data_dir` 默认是解析符号链接后的真实二进制所在目录. 相对 storage 路径使用该目录, 不使用启动目录. 需要把数据放在其他位置时请使用绝对路径.

`storage.database_retention_days` 默认为 90 天, 控制终态 bridge 记录, 未完成启动及符合条件的输入附件的清理. 已准备完成的输入附件在 owner session 近期仍活跃或其状态无法确认时保留; 旧的中断下载也可清理. 需要长期保留的文件请放在 workspace, 不要依赖附件存储.

设置以下内容可关闭自动清理:

```toml
[storage]
database_retention_days = 0
```

Retention cleanup 不会删除:

- OMP session
- workspace, 包括旧版本遗留在其中的输入附件
- 其他 OMP 数据

升级前先停止服务, 备份数据目录, workspace 以及 OMP 自己的 session 存储. 只备份 bridge 数据库不等于完整备份 OMP 对话. 不要删除 SQLite 的 `-wal` 或 `-shm` 文件, 也不要在服务运行时只复制主数据库.

只授权可信用户. 服务使用其 OS 用户的文件权限和环境变量运行; 群成员可能看到提问和回复, 即使没有控制 bot 的权限. 保护好 bot token.

## 日志

日志支持 `text` 和 `json`, 由 `logging.format` 选择. `logging.level` 设置全局级别; `[logging.component_levels]` 可分别覆盖 `daemon`, `bridge`, `rpc`, `telegram`, `store` 或 `media`. 见完整[配置示例](#配置).

日志输出到 stderr, 保存及轮转由进程管理器负责. 不记录 prompt, 模型回复, 凭据, 工具负载或 workspace/session 路径. Chat 和 thread ID 仍可能识别具体对话, 应限制日志访问权限.

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

## 架构

实现及维护细节统一放在[中文架构说明](doc/architecture.zh.md)及[英文版](doc/architecture.md): 命令路由, RPC 完成语义, 持久化, 失败语义, 重启恢复, retention 和日志契约.
