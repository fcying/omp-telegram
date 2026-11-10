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
- 最新稳定版 OMP 及其运行时, 例如 Bun. 兼容目标仅为最新稳定版; 不能仅凭支持 RPC protocol v2 就推定兼容. 使用运行服务的同一用户配置模型和认证.
- Git 仅在使用 OMP 原生 autoresearch 工作流时需要.
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

arm64 使用 `omp-telegram-linux-arm64.txz`. 压缩包包含二进制和 LICENSE, omp 需要单独安装. 可选迁移 Extension 内嵌于二进制. 每个发布版都附带 `SHA256SUMS`.

开发构建发布在 [Development build](https://github.com/fcying/omp-telegram/releases/tag/dev-latest). 这是 prerelease, 可能不稳定.

### 从源码构建

需要 Go 1.27.3 或更新版本 (见 [go.mod](go.mod)) 及 [just](https://github.com/casey/just):

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

可将完整带注释示例 [config.toml](config.toml) 复制到二进制旁, 或从以下最小配置开始. 其他设置使用[配置项参考](#配置项)中的默认值.

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

### 配置文件和环境变量

配置按以下顺序选择:

1. 通过 `--config <路径>` 或 `-c <路径>` 指定的文件.
2. 解析符号链接后的真实二进制所在目录中的 `config.toml`.
3. 默认文件不存在时使用内嵌默认配置.

显式指定的文件不存在, 文件不可读, TOML 无效或已知配置项非法时, 启动会失败. 未知键和 table, 包括旧 flat 字段, 直接忽略, 不迁移也不展开环境变量. 启动和 `--check` 均打印 ERROR 日志, 用行号和列号定位被忽略项, 不打印原始键名或值.

显式相对 `--config` 路径以启动目录为基准. 相对 `storage.data_dir` 和 `storage.workspace_root` 始终以真实可执行文件目录为基准, 不以配置文件目录为基准.

配置字符串支持 `$VAR` 和 `${VAR}`, 但 `[omp.environment]` 的模式与 `allow`/`deny` 变量名为字面值, 不展开引用. `$$` 表示字面美元符号. TOML 解析后其他环境变量引用只展开一次. 默认引用的 `OMP_TELEGRAM_ARGS`, `OMP_TELEGRAM_PROGRESS_MODE` 和 `OMP_TELEGRAM_WORKSPACE_ROOT` 可以未设置. `telegram.progress_mode` 为空或非法时回退到 `summary`. token 和白名单必须提供. 程序不会自动加载 `.env` 文件.
仅用于显示的 `omp.args_labels`, `telegram.status_bar` 字段以及 `telegram.final_actions` 的标签和命令按字面读取, 不展开环境变量.

`telegram.allowed_users` 和 `telegram.allowed_chats` 都是必填项. 只有发送者和 chat 同时命中白名单时才接受 update. 普通私聊的 chat ID 通常等于 user ID; 群组 chat ID 通常是负数.

### 配置项

| 配置项 | 说明 |
| --- | --- |
| `telegram.token` | Telegram bot token. 推荐引用环境变量. |
| `telegram.allowed_users`, `telegram.allowed_chats` | 必填的数字 ID 白名单. 可直接填写整数, 或引用逗号分隔的环境变量值. |
| `telegram.progress_mode` | 任务实时进度: `off`, `summary` 或 `verbose`. 默认读取 `OMP_TELEGRAM_PROGRESS_MODE`; 为空或非法时回退到 `summary`. 见[进度界面](#进度界面). |
| `telegram.progress_recent_tools` | verbose Recent 历史保留的已完成工具上限. 默认 `5`, 范围 `1..100`; 受 Telegram 消息长度限制, 实际显示可能更少. |
| `telegram.final_actions` | 可选的有序完成快捷按钮 `{ label, command }`. 省略或 `[]` 关闭. 最多六项; 标签非空, 单行, 最多 64 个 UTF-16 单位. Command 是一个 slash invocation, 可带固定字面参数, 不能含控制字符或换行. 见[完成快捷按钮](#完成快捷按钮). |
| `telegram.status_bar.progress`, `.intermediate`, `.final` | 按顺序选择 Progress, 中间 assistant 消息和最终结果的字段. 内层数组中的字段用空格分隔, 外层各项用 ` \| ` 分隔. `[]` 关闭对应状态栏. 未配置时 `intermediate` 默认 `[]`, `progress` 和 `final` 默认 `["status", ["thinking", "model", "fast"], "context", "speed"]`. 见[状态栏](#状态栏). |
| `telegram.status_bar.git_command` | 最终 `git` 字段的可选字面 argv. 未配置或 `[]` 使用内置安全 Git 探针. 自定义命令在当前 session workspace 中执行, 不经过 shell; 安全与副作用由用户负责. |
| `telegram.status_bar.git_timeout` | 内置和自定义 Git 采样共用的正数字面 Go duration, 默认 `"450ms"`. 大仓库可增至如 `"2s"`. 空串, 零, 负数, 非法, 溢出或非字符串均拒绝; 不展开环境引用, 不允许关闭超时. |
| `omp.binary` | 从 `PATH` 查找的可执行文件名或绝对路径, 默认 `"omp"`, 不是 shell 命令. |
| `omp.args` | 按顺序排列的 OMP 启动参数预设. 默认只有一个可选的 `OMP_TELEGRAM_ARGS` 预设. 旧的字符串格式仍作为单个预设接受. |
| `omp.args_labels` | 可选的 Telegram 预设标签, 每个预设一个; 无效标签回退为 `Preset N`. 标签对能查看对话的人可见, 不要填写凭据. |
| `omp.migration_extension` | 为所有参数预设在每次 OMP 启动时加载内嵌迁移 Extension, 包括 resume. 默认 `false`, 无需单独安装 Extension. |
| `omp.auto_session_name` | 显式启用新建 session 的一次原生 `/rename` 尝试. 默认 `false`; 可能产生额外模型调用与费用. 见下文. |
| `[sub2api]` | 可选的 bridge 专用上游额度: `base_url`, `api_key` 和精确 OMP `providers`. 全空关闭, 部分配置报错. 见 [sub2api 上游额度](#sub2api-上游额度). |
| `sub2api.extension` | 为所有参数预设启用内嵌 sub2api 额度 Extension. 默认 `false`; 启用额度源时必须显式设为 `true`. |
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

设置 `omp.auto_session_name = true` 后, 仅新建 session 有资格: 首个普通成功任务的 assistant 正文写入 outbox 后, 触发一次原生 `/rename`. 不覆盖已有 name, 不修改 Telegram topic 标题, 不补命名历史 session (包括 resume, fork 和 migrate). 自动命名尽力而为, 可能产生额外模型费用, 失败不保证生成标题; 可手动使用 `/name <title>` 或原生 `/rename`.

如果自动命名状态保存失败, Bot 会通知本次命名已跳过; 任务结果仍已保存, Session 仍可使用. 仅在下一次普通任务成功后重新检查命名.

如果 `/name` 无法确认先前的自动命名已通过原生输入处理, 会回报修改未确认, 不写入请求的名称. 若持续失败, 使用 `/close` 后 `/resume` 重新连接, 再重试 `/name`.

`omp.args` 每项都是一套完整参数预设, 支持 shell-style 引号分组, 但不经过 shell 执行. 除下述保存模型恢复回退外, 只有显式参数或用户命令才会改变 OMP 配置.

`/args` 选择全局预设, 并跨服务重启保存. 切换要求当前对话空闲且没有排队任务. 当前开启的 session 会以新预设及其原生 `default` 模型角色重启; 其他对话在下次启动进程时使用新预设. 已关闭的 session 不会打开. 重启失败仍保留已选预设和原 session. 修改预设值会重置为预设 1, 仅修改标签不会重置.

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
# Optional: off, summary, or verbose.
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
| `/new <名称或路径>` | 在指定目录开启新 session. 替换正在运行的 session 前需要确认. |
| `/new` | 在上次目录开启新 session; 没有历史目录时使用 `storage.workspace_root`. |
| `/stop` | 中止当前任务并清空排队任务, 保留 session 供后续使用. |
| `/queue` | 查看运行及排队任务. Cancel 按钮只删除 bridge 中选中的等待任务, 不中止当前任务或取消原生队列中的 prompt. |
| `/followup <message>` | 明确排队一个独立任务, 不会作为 steer 发送给当前任务. |
| `/close` | 关闭当前 session, 保留文件和 OMP history. |
| `/bindings` | 列出并管理当前 chat 已保存的对话/session 绑定. 图标表示逻辑上的开启, 关闭或待处理状态, 不表示进程是否运行. 删除选项见下文. |
| `/bindings old` | 关闭的在前, 开启的其次, 各组内最久未使用优先; 待处理条目最后. |
| `/resume` | 从当前 workspace 选择已保存的 OMP session, 可 Pin/Unpin, 或经确认后删除. |
| `/resume <session ID>` | 恢复指定的原生 OMP session; OMP 可能恢复该 session 记录的工作目录. |
| `/export` | 选择已保存的 session, 导出原始 main-session JSONL. 导出当前 session 前需等待其空闲. |
| `/export html` | 选择已保存的 session, 导出 standalone HTML viewer. |
| `/export <session ID>` | 不打开 picker, 直接导出指定 session 的原始 OMP main session `.jsonl`. 使用 `/export html <session ID>` 显式选择 HTML. |
| `/status` | 查看当前 workspace, session 及完整原生 ID, model, thinking, fast mode, context, runtime 和队列状态. |
| `/args [预设编号]` | 查看预设标签, 或在空闲时选择预设并立即应用于当前对话已开启的会话. 其他对话在下次启动进程时使用新预设. |
| `/doctor` | 运行安全的 bridge 诊断, 不启动或修改 OMP session. |
| `/name <名称>` | 命名当前 OMP session. 不修改 Telegram topic 名称. |
| `/title <标题>` | 请求 Telegram 重命名当前 topic. 不修改 OMP session 名称. |
| `/model` | 用按钮选择 OMP 配置的 model role. |
| `/model provider/model` | 空闲时切换到指定 model. |
| `/thinking` | 空闲时选择 thinking level. |
| `/fast [on\|ultra\|off\|status]` | 转发 OMP 原生 fast 控制, 任务运行中也可执行. 裸 `/fast` 直接切换开关, 不弹选择器. |
| `/compact` | 空闲时直接压缩当前 context, 无需确认. 状态消息原位更新结果和 context 使用量. |
| `/handoff [补充要求]` | 空闲且队列为空时运行 OMP 原生 handoff, deadline 为 180 秒. |
| `/migrate` | 在同一 workspace 中将 handoff 上下文迁入新 session. 先启用 `omp.migration_extension`. |
| `/review [arguments]` | 作为排队任务运行 OMP 原生 `/review` 命令. |
| `/autoresearch [goal\|off\|clear]` | 当前 OMP 公布支持时控制原生 autoresearch. 安全边界和停止行为见下文. |
| `/lang [en\|zh]` | 查看全局 Bridge 界面语言, 或将所有私聊和 topic 统一切换为英文或中文. |
| `/help` | 查看帮助和当前 bridge 版本 (与 `-v` 一致). |

Topic 的首次 session 会尝试使用 workspace 名称作为标题, 与同一 chat 中已知名称重复时添加 `[thread ID]`. 成功执行 `/title` 后不会再被自动命名覆盖, worker 重建或重启后仍有效. 对于引入此保护前设置的标题, 升级后请再执行一次 `/title`. Telegram 不提供完整 Topic 列表, 未观察到或手动改名的标题可能导致漏判冲突或保留不必要的后缀. 不会重命名其他 Topic.

在要改名的 topic 内使用 `/title Trading research`. 不需要 OMP session, 也不会打断正在运行的任务. 回执仅报告 Bot API 接受或失败, 不表示客户端已显示新标题; 不保证同步 ([上游问题](https://github.com/tdlib/telegram-bot-api/issues/915)). 如果另一次改名尚未完成, 请等待后重试. 在 supergroup 中, bot 需要管理员身份及 `can_manage_topics` 权限, 但它自己创建的非 General topic 除外. General 始终需要这些权限.

Model 或 thinking 修改成功后, 仍可能因无法读取配置等级而显示 `Thinking: n/a`. 这不会撤销修改; 如果必须关闭进程, 回执会说明 session 已保留.

通过 `/model`, `/thinking` 和 `/args` 菜单选择后, 结果原位更新菜单, 不再另发重复的完成通知. 没有菜单或无法更新菜单时, 才单独发送一次结果通知.

`/fast` 立即执行, 任务运行中也可使用. 支持的参数和模型由 OMP 决定. 修改影响后续模型请求, 不改变已经发出的请求; 回执独立于任务答案.

对话空闲时, 普通文字会启动任务. OMP 任务运行期间, OMP 可能接受普通文字作为 steer. 使用 `/followup <message>` 可明确排队一个独立任务. 附件, `/review` 和没有专用控制处理的原生命令进入对话队列并按顺序执行. 不同对话可以并行工作, 受 worker 配置上限影响.

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

Workspace 专属命令在 session 连接后显示, 名称和简介由 OMP 提供. 原生命令输出独立于任务回复发送.

`/lang en` 和 `/lang zh` 为所有私聊及 topic 统一设置 Bridge 界面语言. 任意已授权用户都能修改, 设置跨重启保留. 新 Bridge 消息使用新语言, 命令菜单可能稍后更新. 不翻译已有消息, OMP 回复, 原生交互及简介或用户内容. 可以用任意语言提问.

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

研究循环由原生 OMP 管理, 参见[原生模式说明](https://github.com/can1357/oh-my-pi/blob/v18.10.3/packages/coding-agent/src/autoresearch/prompt-setup.md). 它可能创建分支, 修改文件, 提交保留的实验, 回滚丢弃的实验并多次调用模型. 请使用合适的 workspace, 并考虑费用和文件变更.

Bridge 不会在不同对话之间锁定或隔离 workspace. 使用同一 workspace 的 OMP session 可能修改相同文件; 请使用不同 workspace, 或避免同时运行会修改文件的任务. 服务重启后不会自动重放研究任务; 决定是否重发不确定的任务前, 检查 `/status` 和 workspace.

### 删除已保存的 session

在 `/resume` 中选择 **Delete**, 再用红色 **Delete** 按钮确认. 正在使用的 session 不能删除, 不提供强制删除选项.

在 `/bindings` 中, **Binding** 仅遗忘选中条目, 保留原生历史; **Binding+Session** 删除原生 session, 以及本 bot 各对话中引用它的绑定和 Pin. Session 文件已经缺失时, 只清理失效的 Bridge 引用.

原生 session 删除不可撤销. Workspace 文件和 Telegram topic 保留.

### Session 导出

`/export` 会列出当前 workspace 中保存的 OMP session, 并将选中的原生 `.jsonl` session 文件发送到 Telegram. 默认格式是 OMP 原始 main-session JSONL. `/export html` 则将选中的 session 导出为 standalone HTML viewer.

两种格式都只导出 main session, 不包含 companion 或 subagent transcript, 并受 50 MB document 限制.

导出当前会话前, 请先完成待处理对话和回复. 导出期间的新原生对话不可用, 结束后不会自动重开; 如仍需要, 请明确重新发起对话. 导出其他已保存会话不阻塞当前对话.

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

### 原生 Ask 问题

Bridge 使用 OMP 的 `rpc-ui` 模式, 不改变已启用工具或审批策略. 如果 OMP 已启用原生 `ask`, 可以要求 Agent 使用它, 例如: "使用 Ask 让我选择 Programming, Investing 或 Languages."

从 OMP 18.18.3 起, 原生 `ask` 接收 `{html}` HTML 表单, 不再使用旧 `questions[]` 工具 schema. RPC 没有 `formDialog` 时 (包括 OMP 18.18.6), OMP 通过通用 `select`/`input`/`editor` 请求逐字段提问; checkbox 字段使用多次选择. Bridge 处理这些请求, 不解析 HTML, 也不另建表单渲染器. 请按显示的选项操作, 文字输入须回复对应的专属提示.

文字字段回复保留空白和换行. **使用完整默认值** 接受原生编辑器的预填值; **提交空值** 发送空字符串, 不等同于取消. Placeholder 只是提示, 不是默认值. 必填, 数字和 pattern 校验由 OMP 处理, 无效时可能再次询问同一字段. 较长提示或默认值只在展示时截断; 接受默认值时发送原始内容. 原生 RPC 回退不提供整张表单的布局, 图片或初始 checkbox 勾选标记.

Extensions 仍独立支持基于 questions 的 `ui.askDialog` 和 `set_ask_dialog` opt-in, 通过 RPC `method:"ask"` 传递. 这些扩展组合对话的交互方式为:

- 在同一条消息中逐题显示. 单选点击后立即推进; 多选点击切换选中状态, 再点 **确认选择** 提交. 允许明确确认空多选集合.
- **自定义答案** 打开独立的回复提示. 必须回复该条消息并输入非空文本; 这些组合对话的自定义答案会裁剪首尾空白. 多选可以同时包含选项和自定义文字.

原生表单和扩展对话均遵循:

- 任务等待 Ask 时, 普通指导文字先暂存, 对话/工具结束后再发送给同一个仍在运行的任务. 它不回答或取消 Ask, 也不是独立 followup. 暂存指导共用队列上限; 派发前原任务结束, 被停止或 session/runtime 被替换时, 会取消且不重放. 如仍需要, 请明确重新发送. `/stop` 等控制不在它后面等待.
- 组合对话提供的 **返回选项** 仅取消文字输入. **取消** 取消原生交互; **Stop** 停止任务. 超时和默认答案由 OMP 决定. 旧按钮和回复提示不能回答替换后的问题或 session.
- 回复过期或仍在投递的 Ask 提示时, `/stop` 等 Bridge 命令和原生 `/autoresearch off` 控制仍按正常规则执行. 发给其他 bot 的命令会忽略, 不作为答案.
- 只有发起任务的用户可以回答. 每个对话同时只允许一个原生交互 (Ask, 选择, 确认或文字输入); 额外并发请求会取消, 不替换原菜单. 回答后, 下一对话可能短暂等待上一答案的投递结果确认. 重复请求 ID 不重新打开对话或重放答案. 答案继续原任务, 不创建新 prompt, 失败或重启后不会重放.

OMP 支持 opt-in 时自动启用扩展组合对话; 这不代表启用 HTML `formDialog`. 明确的不支持命令响应会保留 `rpc-ui`, 改用通用选择/纯文本输入; 其他协商失败会中止启动. 不支持标记为 secret 的敏感输入或任意终端编辑器.

不支持在连接前阻塞 OMP 启动的对话: 启动会超时失败, Telegram 不会出现可操作的问题或按钮. 超时时若检测到未应答的确认或其他 RPC 对话, Telegram 会报告该现象, 不将它断言为已证实的原因. 请将扩展配置为非交互启动, 或把提问移至用户调用的命令; 重试前按提示要求使用 `/close`. Bridge 不自动回答启动对话. 参见[原生对话实现](doc/architecture.zh.md#原生对话与-session-operation).

### 常见问题与限制

| 现象 | 检查项 |
| --- | --- |
| bot 没有反应 | 两个白名单, 群组 topic, 群隐私设置, 以及是否有其他 polling 服务或 webhook 占用 bot. |
| `omp.binary executable not found` | 配置的可执行文件和服务进程的 `PATH`, 包括 omp 及其运行时. 它可能不同于交互式 shell. |
| `/resume` 列表为空 | 当前选择的目录以及 OMP 是否已经保存历史. 新 session 可能还没有可恢复文件. |
| `/compact` 失败 | 短 session 可能没有可压缩内容. 如果本机 OMP 也失败, 检查其 model 配置. |
| 数据库结构不支持 | 备份数据, 使用受支持的数据库或新数据目录; 不要手动修改版本号. |

不支持 undo/rewind 和 redo, 语音/转写, 自动创建 topic, 以及任意终端或编辑器界面. 原生 Ask, 选择, 确认和纯文本输入对话按上文支持, 但不是所有交互式工具审批都能远程完成. Bridge 从不自动批准工具操作.

输出过大或回复过多时可能截断, 并明确提示. OMP 原生 session 历史不会被截断.

## 进度界面

`telegram.progress_mode` 控制任务实时进度:

- `off` - 关闭 progress 消息和 typing action.
- `summary` - 显示活动工具, 任务状态及当前 assistant 正文预览.
- `verbose` - 额外显示近期可观察的工具活动. 工具详情不脱敏, 可能含有凭据或私人内容, 能查看对话的人都可见.

Progress 延迟约三秒显示, 同一行带有 **Stop | Follow-up** 按钮. **Stop** 中止当前任务; **Follow-up (后续输入)** 打开回复提示, 不暂停也不 steer 当前任务. 选择 `status` 字段后, **⚙️** 标记与可用状态字段位于同一首行. `/stop` 还会清空等待任务. 进度更新和清理属于尽力而为, 不延迟正式回复. `progress_mode = "off"` 时两个按钮都不创建.

Steer 输入更新当前预览, 不重启 OMP. 正式回复送达后通常会删除预览; 交付失败或不确定时可能保留预览. 进度清理不会删除正式回复.

已完成的 assistant 消息立即发送, 关闭 progress 也不受影响. 选择 `status` 后, 中间状态栏显示 **💬**, 确认任务成功后将最新正式回复标为 **✅**. 取消, 失败, 结果不确定或研究轮次仍在继续时不标成功. 仅首条正式消息引用原始请求; 完成状态更新不重发答案.

### 后续输入

点击 **Follow-up (后续输入)**, 再以非空文本回复 bot 的 ForceReply 提示. 仅接受点击者本人在同一 chat/topic 中对该提示的回复, 与 `/followup <message>` 一样作为独立任务排队. 打开提示不暂停当前任务. 接受后 bot 会确认后续消息已入队; 队列满或空/非文本回复不会消费请求或谎报成功, 请求有效时可以重试.

请求十分钟后过期. 再次点击会替代你在此对话中的旧请求. 原任务自然完成且 session 未变时请求仍有效; Stop, Close, session 替换及服务重启使未完成请求失效. 对过期, 未授权或交付不确定提示的回复一律拒绝, 包括命令; 此时请将命令作为新消息发送. 有效提示中的已知 Bridge 命令保持普通命令效果. 其他普通文字保持原有 prompt/steer 行为. ForceReply 的显示由 Telegram 客户端决定, 不提供 inline 文本输入控件.

### 完成快捷按钮

要启用, 在已有 `[telegram]` table 内, `[telegram.status_bar]` 之前加入:

```toml
final_actions = [
  { label = "Compact", command = "/compact" },
  { label = "New", command = "/new" },
  { label = "Shake", command = "/shake" },
]
```

省略或使用 `final_actions = []` 关闭 (默认关闭). 保留数组顺序, 不翻译标签. 最多六项. 标签必须非空, 单行, 最多 64 个 UTF-16 单位 (BMP 外的 emoji 占两个单位). 命令是一个 slash invocation, 可带固定参数, 不能含换行或控制字符. 标签和命令均为字面值, 不展开环境变量或经过 shell 执行. 不要在可见标签或参数中填写凭据. 每行最多三个按钮, 长标签可能独占一行.

按钮只在任务成功完成后出现在最后一条 assistant 回复上, 关闭状态栏也不影响. 中间回复, 错误/截断提示, 已停止/不确定结果及原生命令输出不显示. 每个按钮只能执行一次; 忙碌, 参数错误或命令不可用时可重新点击. 等待确认时不能重叠点击; 取消或确认超时后可重试. 打开命令菜单视为执行. 已提交或结果不确定的动作不重放, 包括被重启打断的动作. Session 变化及快捷按钮配置删除或变更使旧按钮失效; 其他未使用按钮仍可用.

点击沿用真实点击用户的权限, 以及手动输入命令时的 chat/topic, 参数, 忙碌限制与确认流程. 未知或不可用命令明确报错, 不作为模型 prompt 或替换为其他动作. 原生命令必须在当前 session 可用, 且遵守[原生命令限制](#omp-原生命令). 例如 **Shake** 仅在 OMP 支持时调用 `/shake`, 不提供替代动作.

### sub2api 上游额度

这是状态栏 `quota` 字段可选的数据源, 不是模型 provider 配置. 保持 OMP 现有 provider URL, model/context 设置及凭据不变. Bridge 不会自动修改模型, provider 或全局 OMP 配置. 将以下 table 合并到 bridge 的 `config.toml`:

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

`base_url`, `api_key` 及每个 `providers` 元素沿用单次环境变量展开. 三项全空关闭此源; 部分配置, 非法 HTTP(S) URL, 重复/非法 provider ID, 或未显式启用 `sub2api.extension = true` 时启用数据源, 都以脱敏错误拒绝启动. URL 可含路径前缀, 不得含凭据, query, fragment 或点路径段. 远程管理请使用 HTTPS. Provider 按精确字符串区分大小写匹配, 必须是实际 OMP provider ID, 不能用 model 名, URL 或猜测的 alias. 在 `progress`, `intermediate` 和/或 `final` 独立加入 `quota`; 默认状态栏不变.

管理 key 需要全局 `usage.read` 和目标 OpenAI OAuth 分组上的资源权限 `openai.quota.read`. 全局 `usage.read` 可读取全站日志, 不限于所选分组. 可选资源权限 `accounts.identity.read` 用于显示套餐名; 否则以已确认账号 ID 标记额度.

请保护管理 key, 远程管理使用 HTTPS, 不要复用模型凭据. Key 由 bridge 使用, 不加入 OMP 参数. 若通过环境变量引用 key, 请用[子进程环境策略](#配置项)显式禁止 OMP 继承该变量.

配置校验会拒绝启用 sub2api 且任一 OMP 参数预设包含 `--no-ui` 的组合; 请移除该选项后启用额度. RPC 不需要交互终端. `--no-extensions` 仍与此显式启用的 Extension 兼容.

HTTP 和 SSE 响应支持通过服务端 `x-client-request-id` 响应头确定请求归属. WebSocket 不受支持: `101` 握手无法确定后续每轮实际服务的账号, bridge 不会从握手 ID 推导每轮 usage. Bridge 不修改 OMP 的 transport 配置; WebSocket 请求没有可归属的 sub2api 额度.

任务最终完成且 worker 空闲后, bridge 将该任务的全部 Request ID 解析为实际上游 OpenAI OAuth 账号, 并按账号去重. 同一任务使用多个已配置 provider 时, 这些请求也汇总到同一账号集合. 字段显示其可识别的 5h/7d **已用**百分比和可用的重置倒计时, 格式见下方[状态栏](#状态栏); 多个账号可分别显示. 归属缺失或有歧义, OMP hook/header 不受支持或额度不可用时省略对应值. Bridge 不借用上一任务身份, 不根据 model 猜测账号, 不替换为网关凭据额度.

Usage 日志可能在任务完成后才可查询. 在任务 scope 仍有效且 worker 空闲时, 有界归属重试可回填最新对应的最终 header, 不延迟任务完成, 不修改答案或重放任务. Research 保留实际正文所属轮次的归属, 而非后续空轮. 队列工作, 新 scope 或 runtime 退役优先, 不保证历史 header 必定回填.

原生与 sub2api 额度共用仅 idle 刷新, 缓存和渲染流程, 仅账号归属与额度获取来源不同. 同一账号的刷新尝试受五分钟门控. 刷新中, 读取失败或空结果均保留最近成功样本, 无显示 TTL; 但新任务复用缓存额度前必须确认自己的账号. Key 和原始响应不记日志, 不发送 Telegram; 详细 `/status` 不变. 实现细节见[架构文档](doc/architecture.zh.md#sub2api-额度).

### 状态栏

三种状态栏独立配置, 按数组顺序显示字段. 外层各项用 ` | ` 分隔, 内层数组中的字段用空格分隔, 仍支持原来的一维数组. 无数据的字段及空组不留下分隔符; 未知名称及重复字段会报错, 包括跨组重复. `[]` 删除整个状态栏, 包括图标和分隔符. 关闭 Progress 状态栏不隐藏工具正文, 文本预览或 **Stop | Follow-up** 按钮; 关闭整个 Progress 仍使用 `progress_mode = "off"`.

```toml
[telegram.status_bar]
progress = ["status", "model", "context"]
intermediate = []
final = ["status", ["thinking", "model", "fast"], ["context", "speed"], "quota", "git"]
```

| 字段 | 显示内容 |
| --- | --- |
| `status` | 处理中, assistant 或已确认完成的图标. |
| `model` | 字面 model ID, 不带 provider. |
| `thinking` | 配置的 thinking selector 符号. |
| `fast` | 已确认的 fast mode 活动/设置. |
| `context` | 已确认的上下文窗口已用百分比及窗口容量, 如 `46% (500k)`. |
| `speed` | 原生实时速度或已确认回复的平均速度, 如 `3.8 t/s`. |
| `quota` | 原生或真实上游账号额度的已用百分比, 附带可用的重置倒计时. |
| `git` | 仅最终结果: 当前分支和可选的 dirty `*`, 或自定义命令 stdout. |

没有数据或尚未读取的字段直接省略, 不显示 `ctx ?`, `quota ?` 或 `n/a` 占位. 额度刷新失败保留已确认的 last-good 样本. 已确认的数据可以立即显示; 刷新不回填较早的中间回复. 状态栏过长时省略整个字段, 不截断值. 详细 `/status` 不变.

`context` 和 `quota` 均显示**已用百分比**, 不是剩余比例: 数值越高越接近上限. `context` 表示上下文窗口占用, `quota` 表示账号在各限额窗口内的用量. Quota 双窗口 `20%(2h) 60%(5d12h)` 表示 5 小时额度已用 20%, 2 小时后重置; 7 天额度已用 60%, 5 天 12 小时后重置. 单窗口保留标签, 如 `7d 60%(5d12h)`. `~` 标记粗略重置时间, 如 `(~6d)`; 精确倒计时不足一小时显示 `(<1h)`. 重置时间缺失或已过时只省略倒计时, 不假定额度已经恢复. 倒计时在状态栏渲染时更新, 不在历史消息中持续走时.

`git` 显示任务完成时当前 workspace 的分支: 干净时为 `main`, 有 staged, unstaged 或未忽略的 untracked 改动时为 `main*`, detached HEAD 为 `HEAD`. 仅 ahead/behind 不算 dirty. 非 Git 仓库, 安全读取失败或超时时省略字段, 包括内容过滤器使安全读取不可用的仓库. 研究持续期间不显示新的 Git 采样, 直到研究结束.

配置非空 `git_command` argv 可覆盖 `git` 字段:

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

自定义命令只在 `final` 选择 `git` 时执行, 使用当前 workspace 和 `omp.environment` 策略. 参数保持字面值, 不经过 shell 或展开环境变量. 与内置探针不同, 自定义命令没有 Git 专用安全保护; 请使用安全的只读命令, 不要依赖恰好执行一次. Stdout 必须是非空的有效 UTF-8 单行文本, 去掉末尾行结束符后裁剪至 64 字节. 失败, 超时, 非法输出或输出超过 64 KiB 时省略字段, 不回退到内置探针. 后续额度更新不会重新执行命令.

`git_timeout` 默认 `"450ms"`, 大仓库可增至如 `"2s"`. 必须是正数字面 Go duration 字符串, 不展开环境引用, 不允许关闭超时. 增大超时不放宽安全或输出限制, 字段仍可能省略. Git 采样细节见[架构文档](doc/architecture.zh.md#git-状态采样).

迁移: 旧 `telegram.provider_usage` 键会被忽略并打印 ERROR 日志, 不再阻止启动. 请删除它, 仅在需要显示额度的数组中加入 `"quota"`; 三处都不选则关闭额度读取. 默认不选择 quota 和 Git.

额度独立于 progress 模式, 但仅在已关闭的 Progress 中选择 quota 不会启动读取. 短任务结束后仍可能显示可用额度, 不延迟任务完成.

回复支持 Markdown, 长文本自动分片. 大表格可能改用纯文本布局; Telegram 不支持的格式回退为可读文本.

#### 原生 provider 额度

选择 `quota` 后, 对应状态栏可显示原生 Claude (`anthropic`) 或 OpenAI Codex (`openai-codex`) 额度, 独立于 progress 模式. 展示可识别的 5h/7d 已用百分比及 OMP 提供的粗略重置时间, 不附加读取时间. 账号身份, 邮箱, credential ID 和原始报告不发送到 Telegram, 不写入日志或数据库.

额度仅在 OMP 空闲时尽力刷新, 排队 prompt 和控制操作优先. 忙碌或长任务有缓存就显示, 否则省略字段. 缓存值可能不是最新值; 查询失败或结果为空时保留最近成功读取的值. 刷新成功后, 若 final 选择了 `quota`, 可以更新最近的最终状态栏, 不修改或重发答案及其 Git 结果. 服务重启会清除缓存.

账号信息未知或有歧义时省略额度, 不展示其他账号的值. 凭据轮换时, 原生额度无法识别每次请求实际计费的账号. 已配置 sub2api 的 provider 改用[上游额度源](#sub2api-上游额度). 原生账号与刷新机制见[架构文档](doc/architecture.zh.md#原生-provider-额度).

## 附件

使用 Telegram 附件按钮发送照片或文档, 在 caption 中说明要让 OMP 做什么; 没有 caption 时仍会将附件元数据作为一次任务提交给 OMP, 但 bridge 不补充具体任务指令. Photo 或 document 相册作为一个任务处理, 最多 10 个文件, 使用第一个非空 caption.

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

普通恢复保留已保存的模型. 如果 OMP 明确报告无法恢复该模型, Bridge 会使用所选配置的 `default` 模型角色重试一次并提示回退. 不修改保存的参数预设或 OMP 配置. 其他启动失败不会自动重试.

中断或结果不确定的工作不会自动重放. 停机或崩溃时, 等待中的任务会取消; 决定重发前先检查聊天和 workspace.

`/stop` 结束活动任务但保留 session. `/close` 关闭 session, 保留 workspace 文件和 OMP history. 如果 `/new` 或 `/resume` 中断, 重试前先使用 `/close`.

每个对话有独立 session, 但使用同一 workspace 的 session 会共享文件. 不同对话不是文件系统或凭据沙箱.

### 迁移大型 session

`/migrate` 使用模型生成的 handoff 摘要, 在同一 workspace 开启较小的新 session, 保留 model, thinking 和 fast mode 设置. 旧 session 仍可使用. Handoff 最多等待 180 秒, 短会话也支持迁移.

在现有 `[omp]` 表中启用内嵌迁移 Extension, 然后重启 bridge:

```toml
[omp]
migration_extension = true
```

迁移要求 session 空闲且队列为空, 内嵌 Extension 无需单独安装. 如果之前通过 `-e` 手动加载了该 Extension, 请移除该参数, 避免重复加载. 其他 Extension 命令和别名仍不支持.

迁移失败, 超时或中断后, 普通输入保持锁定. `/stop` 和 `/close` 停止 runtime, 但不会解除迁移锁. 检查 `/status` 后, 显式恢复源 session 或已知候选:

```text
/migrate recover old
/migrate recover <full-session-id>
/migrate recover <absolute-session-file>
```

恢复不会重跑 handoff 或创建新 session. 候选未知时使用 `/migrate recover old`, 不要猜测最新文件. 迁移的是摘要文本, 不是活动工具, eval 状态或全部 session artifacts.

## 数据与保留

Bridge 数据保存在 `<storage.data_dir>/omp-telegram.db`, 包括已接受的 Telegram 输入内容, 待发送/已发送回复和消息 metadata. 附件以文件形式保存在同一数据目录下. 数据库, 附件缓存及其备份都应按敏感数据保护. `storage.data_dir` 默认是解析符号链接后的真实二进制所在目录; 相对 storage 路径使用该目录, 不使用启动目录.

`storage.database_retention_days` 控制 bridge 终态消息, 符合清理条件的附件缓存及过期 Follow-up 请求 metadata 的保留时间. 默认 `90` 天; `0` 关闭自动保留清理. 待处理工作及交付或进度清理仍需使用的记录可能保留更久. 不删除 workspace 文件, OMP session 数据或已保存的对话 binding. 保留清理不会删除已送达的 Telegram 回复.

升级前先停止服务, 备份数据目录, workspace 及 OMP 自己的 session 存储. 只备份 bridge 数据库不等于完整备份 OMP 对话.

只授权可信用户. 服务使用其 OS 用户的文件系统权限和环境运行; 群成员即使无权控制 bot, 也可能看到 prompt 和回复. 请保护 bot token.

## 日志

日志支持 `text` 和 `json`, 由 `logging.format` 选择. `logging.level` 设置全局级别; `[logging.component_levels]` 可分别覆盖 `daemon`, `bridge`, `rpc`, `telegram`, `store` 或 `media`. 见带注释的[配置示例](config.toml).

日志输出到 stderr, 保存及轮转由进程管理器负责. 不记录 prompt, 模型回复, 凭据, 工具负载或 workspace/session 路径. Chat 和 thread ID 仍可能识别具体对话, 应限制日志访问权限.

`rpc_queue_overflow` ERROR 和 `rpc_consumer_slow` WARN 表示 RPC 压力或 bridge 操作缓慢. 重发中断或结果不确定的工作前, 请检查 `/status` 和 workspace; 这些工作不会自动重放. 保留附近日志用于诊断, 但仅凭这些告警不能确定原因. 诊断字段解读见[架构日志契约](doc/architecture.zh.md#日志契约).

## 开发

贡献者工作流, 验证及发布命令见[开发, 验证与发布](doc/architecture.zh.md#开发-验证与发布).

## 架构

实现, 失败处理和验证细节见[中文架构说明](doc/architecture.zh.md)及[英文版](doc/architecture.md).
