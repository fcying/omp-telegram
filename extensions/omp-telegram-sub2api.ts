import type { ExtensionAPI } from "@oh-my-pi/pi-coding-agent";

const marker = "omp-telegram.sub2api.v1 ";
const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;
const maxNotificationBytes = 2048;

function identity(value: unknown): value is string {
	return typeof value === "string" && value.length > 0 && Buffer.byteLength(value) <= 128 &&
		!/[\p{Cc}\p{Cf}\p{Z}\s]/u.test(value);
}

// Explicitly loaded with sub2api.extension; never persist response metadata.
export default function sub2apiExtension(pi: ExtensionAPI): void {
	pi.on("after_provider_response", (event, ctx) => {
		try {
			if (!Number.isInteger(event.status) || event.status < 200 || event.status >= 300) return;
			const clientRequestId = event.headers?.["x-client-request-id"];
			if (typeof clientRequestId !== "string" || !uuid.test(clientRequestId)) return;
			// The response hook scopes ctx.model to the model that made this request.
			const provider = ctx.model?.provider;
			const sessionId = ctx.sessionManager.getSessionId();
			if (!identity(provider) || !identity(sessionId)) return;
			const message = marker + JSON.stringify({ sessionId, provider, clientRequestId });
			if (Buffer.byteLength(message) > maxNotificationBytes) return;
			// RPC notify works without tools or an interactive UI. Keep no state across sessions.
			ctx.ui.notify(message, "info");
		} catch {
			// Observation must not affect provider calls or expose native exception data.
		}
	});
}
