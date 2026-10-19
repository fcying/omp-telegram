import { createHash } from "node:crypto";
import { isAbsolute } from "node:path";
import { settings, type ExtensionAPI } from "@oh-my-pi/pi-coding-agent";
import { cfgCompactionKeepRecentTokens } from "@oh-my-pi/pi-coding-agent/session/context-settings";

const command = "omp_telegram_migrate_v1";
const markerType = "omp-telegram.migration.complete";
const maxSummaryBytes = 128 * 1024;
const maxArgumentBytes = 900 * 1024;

type Request = {
	version: number;
	migrationId: string;
	sourceSessionId: string;
	sourceGeneration: number;
	workspace: string;
	summary: string;
	summarySha256: string;
};

function identity(value: unknown, limit: number): value is string {
	return typeof value === "string" && value.length > 0 && Buffer.byteLength(value) <= limit && !/[\r\n\0]/.test(value);
}

function parseRequest(args: string): Request {
	if (Buffer.byteLength(args) > maxArgumentBytes) throw new Error("迁移请求无效.");
	const value: unknown = JSON.parse(args);
	if (value === null || typeof value !== "object") throw new Error("迁移请求无效.");
	const r = value as Request;
	if (
		r.version !== 1 || !identity(r.migrationId, 128) || !identity(r.sourceSessionId, 128) ||
		!Number.isSafeInteger(r.sourceGeneration) || r.sourceGeneration < 0 ||
		!identity(r.workspace, 4096) || !isAbsolute(r.workspace) ||
		typeof r.summary !== "string" || r.summary.length === 0 || Buffer.byteLength(r.summary) > maxSummaryBytes ||
		typeof r.summarySha256 !== "string" || !/^[a-f0-9]{64}$/.test(r.summarySha256) ||
		createHash("sha256").update(r.summary, "utf8").digest("hex") !== r.summarySha256
	) throw new Error("迁移请求无效.");
	return r;
}

// Load with omp.migration_extension; this command never submits a model turn.
export default function migrationExtension(pi: ExtensionAPI): void {
	let budget: { migrationId: string; sourceSessionId: string; workspace: string; restore?: () => void } | undefined;
	const restoreBudget = (): void => {
		const current = budget;
		if (current?.restore) {
			current.restore();
			current.restore = undefined;
		}
	};
	pi.on("session_compact", restoreBudget);
	pi.on("session_shutdown", restoreBudget);
	pi.registerCommand("omp_telegram_migrate_handoff_v1", {
		description: "Telegram 迁移临时 handoff 预算(仅供桥接器调用)",
		handler: async (args, ctx) => {
			try {
				if (Buffer.byteLength(args) > 8192) throw new Error("Invalid request");
				const request = JSON.parse(args) as {
					migrationId: string; sourceSessionId: string; workspace: string; action: string;
				};
				if (!request || !identity(request.migrationId, 128) || !identity(request.sourceSessionId, 128) ||
					!identity(request.workspace, 4096) || !isAbsolute(request.workspace) ||
					ctx.sessionManager.getSessionId() !== request.sourceSessionId ||
					ctx.sessionManager.getCwd() !== request.workspace || ctx.cwd !== request.workspace ||
					ctx.hasPendingMessages()) throw new Error("Invalid source");
				if (request.action === "begin") {
					if (budget?.restore) throw new Error("Budget already active");
					const previous = cfgCompactionKeepRecentTokens.get(settings);
					const runtime = settings.getProvenance(cfgCompactionKeepRecentTokens) === "runtime";
					budget = {
						migrationId: request.migrationId, sourceSessionId: request.sourceSessionId, workspace: request.workspace,
						restore: () => {
							if (runtime) cfgCompactionKeepRecentTokens.override(settings, previous);
							else cfgCompactionKeepRecentTokens.clearOverride(settings);
						},
					};
					// Runtime-only: never persist a migration budget to user configuration.
					cfgCompactionKeepRecentTokens.override(settings, 0);
				} else if (request.action === "restore") {
					if (!budget || budget.migrationId !== request.migrationId ||
						budget.sourceSessionId !== request.sourceSessionId || budget.workspace !== request.workspace) {
						throw new Error("Budget owner mismatch");
					}
					restoreBudget();
				} else throw new Error("Invalid action");
			} catch {
				throw new Error("迁移临时预算未确认; 请在 Telegram 中查看迁移状态并显式恢复.");
			}
		},
	});
	pi.registerCommand(command, {
		description: "Telegram 受控会话迁移(仅供桥接器调用)",
		handler: async (args, ctx) => {
			try {
				if (budget?.restore) throw new Error("Migration budget not restored");
				const request = parseRequest(args);
				if (ctx.sessionManager.getSessionId() !== request.sourceSessionId || ctx.cwd !== request.workspace ||
					ctx.sessionManager.getCwd() !== request.workspace || ctx.hasPendingMessages()) {
					throw new Error("迁移来源不匹配.");
				}
				const parentSession = ctx.sessionManager.getSessionFile();
				const model = ctx.model;
				if (!parentSession || !model) throw new Error("迁移来源不可用.");
				const completion: { run?: () => Promise<void> } = {};
				const result = await ctx.newSession({
					parentSession,
					setup: async manager => {
						const targetSessionId = manager.getSessionId();
						const targetFile = manager.getSessionFile();
						if (targetSessionId === request.sourceSessionId || !targetFile || manager.getCwd() !== request.workspace) {
							throw new Error("迁移目标不匹配.");
						}
						// Persist the model for native resume, not only the current runtime.
						manager.appendModelChange(`${model.provider}/${model.id}`);
						const summaryEntryId = manager.appendMessage({
							role: "user", content: [{ type: "text", text: request.summary }], timestamp: Date.now(),
						});
						await manager.flush();
						// Capture the writable manager only through the supported setup API.
						completion.run = async () => {
							if (manager.getSessionId() !== targetSessionId || manager.getSessionFile() !== targetFile ||
								ctx.sessionManager.getSessionId() !== targetSessionId || ctx.sessionManager.getSessionFile() !== targetFile ||
								manager.getCwd() !== request.workspace || ctx.cwd !== request.workspace) {
								throw new Error("迁移目标已改变.");
							}
							// This attests only that reload returned. The bridge checks live context independently.
							pi.appendEntry(markerType, {
								version: request.version, migrationId: request.migrationId,
								sourceSessionId: request.sourceSessionId, sourceGeneration: request.sourceGeneration,
								targetSessionId, workspace: request.workspace, summaryEntryId,
								summarySha256: request.summarySha256, reloadCompleted: true,
							});
							await manager.flush();
						};
					},
				});
				const complete = completion.run;
				if (result.cancelled || !complete) throw new Error("迁移未完成.");
				await ctx.reload();
				await complete();
			} catch {
				// Never expose JSON arguments, summaries, paths or native exception text.
				throw new Error("受控迁移未完成; 请在 Telegram 中查看迁移状态并显式恢复.");
			}
		},
	});
}
