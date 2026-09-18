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
token = "${OMP_TELEGRAM_BOT_TOKEN}"
allowed_users = ["${OMP_TELEGRAM_ALLOWED_USERS}"]
allowed_chats = ["${OMP_TELEGRAM_ALLOWED_CHATS}"]
omp = "omp"
omp_args = "${OMP_TELEGRAM_ARGS}"
data_dir = "."
workspace_root = "${OMP_TELEGRAM_WORKSPACE_ROOT}"
max_workers = 4
queue_capacity = 16
progress_mode = "summary"
database_retention_days = 90
```

未指定配置文件, 且默认文件不存在时, 程序会使用同样的内置默认配置, 不生成文件.
`database_retention_days` 启用 Database Message Retention janitor. 它按最后一次状态转换时间保留设定天数的终态 inbox/outbox 记录. 默认值是 `90`; 设置为 `0` 可关闭清理. pending/submitted inbox 及 pending/sending outbox 保持持久化. 它绝不删除 binding、history、startup intent、工作目录、omp session 文件或其他 omp 数据.

`progress_mode` 控制一条尽力而为、可编辑的任务实时消息: `off` 关闭它和 typing action, `summary` 显示 assistant 输出、活动工具名和任务状态, `verbose` 额外显示最近 6 条可观察的工具活动. 实时进度绝不包含 reasoning、工具参数、命令文本、结果、stdout 或 stderr. 它不持久化, 不影响最终回复的可靠交付.

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

## 对话命令

| 命令 | 用途 |
| --- | --- |
| `/new <名称或路径>` | 在指定目录开启新会话, 替换已有实例前需要确认 |
| `/new` | 沿用当前对话上次选择的目录, 开启新会话 |
| `/stop` | 中止当前任务并清空排队消息, 保留会话 |
| `/close` | 关闭 omp 实例, 保留文件和会话历史 |
| `/resume` | 用分页按钮选择当前目录中的 omp 历史会话 |
| `/resume <session ID>` | 按 omp 原生 ID 恢复会话及原目录, 使用前先关闭已有实例 |
| `/status` | 查看工作目录, session ID, 模型, 运行状态和队列 |
| `/model` | 查看当前状态和模型 |
| `/model provider/model` | 空闲时切换模型 |
| `/compact` | 空闲时经确认压缩上下文 |
| `/review [arguments]` | 作为独立任务运行 omp 原生 `/review` 命令 |
| `/help` | 查看帮助 |

以上所有命令, 包括 `/new <名称或路径>` 和 `/resume`, 都可用于普通私聊和 topic. 普通私聊使用 `(chat, 0)`, 沿用 topic 的 worker 和会话生命周期, 不需要单独的私聊 worker 或数据库迁移. `/followup` 仍不支持.

普通文字, 附件和 `/review` 都是由 bridge 排队的独立任务, 在每个对话内串行执行. 任务运行期间发送的消息会等待当前任务结束. `/stop` 先清空 bridge 队列, 再发送普通 `abort` 请求中止当前任务.

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

显式指定的文件不存在, 文件不可读或 TOML 格式错误时都会报错. 上面的默认配置已包含必填白名单.

| 配置项 | 说明 |
| --- | --- |
| `token` | Bot token, 推荐引用环境变量 |
| `allowed_users`, `allowed_chats` | 必填的数字 ID 白名单, 可直接填写整数, 或引用逗号分隔的环境变量值 |
| `omp` | 从 `PATH` 查找的可执行文件名, 或绝对路径, 不是 shell 命令 |
| `omp_args` | omp 额外参数, 默认读取可选的 `OMP_TELEGRAM_ARGS`; 显式空字符串禁用额外参数 |
| `data_dir` | 数据库, 锁和待发送附件的存储目录, 默认二进制所在目录 |
| `workspace_root` | `/new <名称>` 使用的根目录, 默认读取可选的 `OMP_TELEGRAM_WORKSPACE_ROOT`, 再回退到二进制旁的 `workspace/` |
| `max_workers` | 同时运行的对话实例上限, 默认 4 |
| `queue_capacity` | 每个对话的等待消息上限, 默认 16 |

字符串支持 `$VAR` 和 `${VAR}`, `$$` 表示字面美元符号. 默认配置对 `OMP_TELEGRAM_ARGS` 和 `OMP_TELEGRAM_WORKSPACE_ROOT` 的引用允许未设置, 其他缺失引用会报错. 程序不会自动加载 `.env` 或 shell 启动文件.

使用其他位置的桥接配置:

```sh
~/tool/omp-telegram/omp-telegram -c "$HOME/.config/omp-telegram/config.toml" --check
```

### 给 omp 传入参数

例如, 使用你已经准备好的 omp 配置 overlay:

```sh
export OMP_TELEGRAM_ARGS="--config \"$HOME/.config/omp/telegram.yml\""
```

这是 **omp 的配置**, 不是桥接配置. 也可以直接在 TOML 中设置 `omp_args`. 参数支持引号, 但不会经过 shell 执行; 配置文件请使用绝对路径. 桥接不会自动修改 omp 的配置, 凭据, 工具或审批策略. RPC 模式, 工作目录及会话生命周期参数由桥接管理, 不能自行覆盖.

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

**相对 `data_dir` 和 `workspace_root` 均以二进制目录为基准**, 不是启动目录或配置文件所在目录. 移动二进制可能会使用另一份数据库, 数据需要独立存放时请使用绝对路径. 显式传入的相对 `--config` 路径是例外, 它相对于调用目录.

升级前先停止服务, 备份数据目录, 工作目录和 omp 自己的会话存储. 只备份桥接数据库并不等于备份完整 omp 对话. 不要删除 SQLite 的 `-wal`/`-shm` 文件, 也不要在写入期间只复制主数据库. 旧的无版本开发数据库不会自动升级; 若启动提示结构不支持, 先备份并使用新的数据目录.

用 `~/tool/omp-telegram/omp-telegram --version` 或 `-v` 查看安装版本.

- 只授权可信用户. omp 拥有服务用户的文件访问权限和环境变量, 不同对话的独立会话不是文件系统或凭据沙箱.
- 群成员可能看到提问和回复, 即使他们无权控制 bot. 数据库也会保存消息内容, 目前没有自动清理保留期.
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

开发者请参阅[架构与开发说明](doc/architecture.zh.md).
