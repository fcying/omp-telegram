# Project Guidelines

## Goal

Build a Go Telegram daemon that manages an independent `omp --mode rpc` process for each topic. Follow architecture.md for design and acceptance criteria.

## Communication

Use Chinese for conversations and explanations, and English for code comments, this file, and the default README.md. Maintain the Chinese translation in README.zh.md, with links between the two README files. Keep both versions consistent. Use ASCII punctuation. State assumptions and acceptance criteria before making changes.

## Engineering

- Keep the implementation minimal. Do not parse PTY/ANSI output or introduce leader/follower coordination or a generic backend framework.
- Identify topics by bot ID, chat ID, and thread ID. Persist session paths as session identities, not PIDs.
- Process ordinary prompts sequentially within each topic and concurrently across topics. Control commands must not wait behind the prompt queue.
- Read RPC stdout continuously, independently of Telegram network delivery. Distinguish request acceptance from task completion.
- Preserve omp's own session history. Do not maintain a second copy of model context.
- Own only process startup, RPC communication, and shutdown. Do not modify omp configuration or automatically override its models, credentials, tools, approval policies, extensions, or session storage. Apply settings only through explicitly configured `omp_args` or user-requested RPC commands; pass argv directly without a shell and preserve bridge-owned RPC/session lifecycle options.
- Attachment delivery may register the bridge-owned `telegram_send` RPC host tool. Keep file access confined to the current worker workspace, authorize downloads before network access, and never change omp's approval policy or infer attachments by scanning directories.
- Keep bridge defaults (`config.toml`, `omp-telegram.db`, lock, and workspaces) relative to the real executable directory after resolving symlinks, never the launch working directory. Explicit relative `-config` paths remain caller-relative. Keep `omp` resolved through PATH by default and leave omp's own defaults untouched.
- Events and callbacks from an old generation must not affect a new worker.
- Never automatically replay tasks with uncertain execution status or promise exactly-once execution.
- Launch processes with explicit argument arrays, not shell string concatenation. Do not enable automatic tool approval by default.
- Authorize every message and callback. Never write tokens, authentication headers, system prompts, or raw RPC state to logs or Telegram.
- Do not equate process isolation with filesystem or credential isolation. Do not delete user workspaces or historical sessions.

## Verification

- Exercise the affected scenario after behavioral changes. Prefer smoke tests against a real omp RPC process.
- Keep tests that prevent observable behavioral regressions. Do not test source text or trivial field forwarding.
- Run gofmt after integration. Use `just` for all subsequent builds and test execution: `just` or `just build` compiles, `just test` runs unit tests without starting or restarting a service, `just check` runs unit tests, race checks, and vet, and `just deploy` installs then restarts the existing supervisor service. For configuration validation, build with `just build` then run `./omp-telegram --check`. Do not bypass these recipes with direct Go build/test commands.
- If Telegram credentials are unavailable, report unverified scenarios explicitly. Never fabricate live verification results.
