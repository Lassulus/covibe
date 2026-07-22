// covibe-collab — omp extension that streams the running session to covibe's
// own collab relay (the covibe dashboard), so remote viewers see it live in the
// browser and can prompt/interrupt. Loaded by covibe via `omp -e`.
//
// covibe owns the relay + viewer, so there is no room key to mint here: the
// wrapper hands us a pre-authorized host URL in COVIBE_COLLAB_URL
// (ws://<dashboard>/collab/host/<id>?token=<hostToken>). We connect, forward
// normalized session events, and inject guest prompts.
//
// Event shapes are read defensively via narrowing guards rather than omp's
// internal event types, so this keeps working across omp versions.

import type { ExtensionAPI } from "@oh-my-pi/pi-coding-agent";
import { appendFileSync } from "node:fs";

// ---- normalized frames we emit (contract owned here) ----------------------

interface EvMsg {
	k: "msg";
	id: string;
	role: "assistant" | "user";
	name?: string;
}
interface EvText {
	k: "text";
	id: string;
	delta: string;
}
interface EvThink {
	k: "think";
	id: string;
	delta: string;
}
interface EvMsgEnd {
	k: "msgend";
	id: string;
}
interface EvTool {
	k: "tool";
	callId: string;
	name: string;
	input: unknown;
	status: "start";
}
interface EvToolEnd {
	k: "toolend";
	callId: string;
	result: string;
	ok: boolean;
}
interface EvStatus {
	k: "status";
	text: string;
}
type Ev = EvMsg | EvText | EvThink | EvMsgEnd | EvTool | EvToolEnd | EvStatus;

// ---- narrowing helpers (no casts) -----------------------------------------

function isRecord(v: unknown): v is Record<string, unknown> {
	return v !== null && typeof v === "object";
}
function pickStr(v: unknown, key: string): string | undefined {
	if (!isRecord(v) || !(key in v)) return undefined;
	const x = v[key];
	return typeof x === "string" ? x : undefined;
}
function pickObj(v: unknown, key: string): unknown {
	if (!isRecord(v) || !(key in v)) return undefined;
	return v[key];
}
// messageText extracts text already present on a message (user prompts and
// pre-filled content arrive whole, not as streamed text deltas).
function messageText(message: unknown): string {
	const content = pickObj(message, "content");
	if (typeof content === "string") return content;
	if (Array.isArray(content)) {
		let out = "";
		for (const part of content) {
			if (pickStr(part, "type") === "text") out += pickStr(part, "text") ?? "";
		}
		return out;
	}
	return pickStr(message, "text") ?? "";
}
function isTruthy(v: unknown): boolean {
	return v === true || v === "true" || v === 1;
}

// Summarize a tool result into display text without asserting a shape.
function summarizeResult(result: unknown): string {
	if (typeof result === "string") return result;
	const content = pickObj(result, "content");
	if (Array.isArray(content)) {
		const parts: string[] = [];
		for (const item of content) {
			const t = pickStr(item, "text");
			if (t !== undefined) parts.push(t);
		}
		if (parts.length > 0) return parts.join("");
	}
	const text = pickStr(result, "text");
	if (text !== undefined) return text;
	try {
		return JSON.stringify(result);
	} catch {
		return String(result);
	}
}

// ---- plugin ---------------------------------------------------------------

export default function covibeCollab(pi: ExtensionAPI): void {
	const url = process.env.COVIBE_COLLAB_URL ?? "";
	if (url === "") return; // not launched under covibe; no-op

	const sessionName = process.env.COVIBE_SESSION_NAME ?? pi.getSessionName() ?? "session";
	const cwd = process.cwd();

	let ws: WebSocket | undefined;
	let connected = false;
	let closed = false;
	let backoff = 500;
	const queue: string[] = [];
	let lastCtx: unknown;
	let msgSeq = 0;
	let curMsgId = "";
	const dbgPath = process.env.COVIBE_COLLAB_DEBUG ?? "";
	let emitted = 0;

	const log = (m: string): void => {
		try {
			pi.logger.info(`[covibe-collab] ${m}`);
		} catch {
			/* logger unavailable during some phases */
		}
		if (dbgPath !== "") {
			try {
				appendFileSync(dbgPath, `${new Date().toISOString()} ${m}\n`);
			} catch {
				/* ignore */
			}
		}
	};
	log("loaded");

	const raw = (frame: unknown): void => {
		let s: string;
		try {
			s = JSON.stringify(frame);
		} catch {
			return;
		}
		if (ws !== undefined && connected) {
			ws.send(s);
			return;
		}
		queue.push(s);
		if (queue.length > 5000) queue.shift();
	};
	const emit = (ev: Ev): void => {
		emitted++;
		if (emitted <= 25) log(`emit ${ev.k}`);
		raw({ t: "ev", e: ev });
	};

	const handleControl = (data: unknown): void => {
		if (typeof data !== "string") return;
		let msg: unknown;
		try {
			msg = JSON.parse(data);
		} catch {
			return;
		}
		const t = pickStr(msg, "t");
		log(`ctrl ${String(t)}`);
		if (t === "prompt") {
			const text = pickStr(msg, "text");
			if (text !== undefined && text !== "") {
				try {
					pi.sendUserMessage(text);
					log("sendUserMessage ok");
				} catch (err) {
					log(`prompt failed: ${String(err)}`);
				}
			}
		} else if (t === "abort") {
			if (isRecord(lastCtx) && "abort" in lastCtx && typeof lastCtx.abort === "function") {
				try {
					lastCtx.abort();
				} catch (err) {
					log(`abort failed: ${String(err)}`);
				}
			}
		}
	};

	const connect = (): void => {
		if (closed) return;
		ws = new WebSocket(url);
		ws.onopen = (): void => {
			connected = true;
			backoff = 500;
			raw({ t: "hello", name: sessionName, dir: cwd });
			const pending = queue.splice(0, queue.length);
			for (const s of pending) ws?.send(s);
			log("connected to relay");
		};
		ws.onmessage = (e: MessageEvent): void => handleControl(e.data);
		ws.onerror = (): void => {
			log("ws error");
		};
		ws.onclose = (): void => {
			connected = false;
			ws = undefined;
			log("ws close");
			if (closed) return;
			const wait = Math.min(backoff, 15000);
			backoff = Math.min(backoff * 2, 15000);
			setTimeout(connect, wait + Math.floor(Math.random() * 250));
		};
	};

	// ---- event taps (registered at load; connect happens on session_start) --

	pi.on("session_start", (_event, ctx): void => {
		lastCtx = ctx;
		log("session_start");
		if (ws === undefined) connect();
	});
	pi.on("session_shutdown", (): void => {
		closed = true;
		try {
			ws?.close();
		} catch {
			/* ignore */
		}
	});

	const remember = (ctx: unknown): void => {
		lastCtx = ctx;
	};

	pi.on("message_start", (event, ctx): void => {
		remember(ctx);
		const message = pickObj(event, "message");
		const id = pickStr(event, "messageId") ?? pickStr(event, "id") ?? pickStr(message, "id") ?? `m${++msgSeq}`;
		const role = (pickStr(message, "role") ?? pickStr(event, "role")) === "user" ? "user" : "assistant";
		curMsgId = id;
		emit({ k: "msg", id, role });
		const initial = messageText(message);
		log(`msgstart role=${role} textlen=${initial.length}`);
		if (role === "user" && initial !== "") emit({ k: "text", id, delta: initial });
	});

	pi.on("message_update", (event, ctx): void => {
		remember(ctx);
		if (curMsgId === "") {
			curMsgId = `m${++msgSeq}`;
			emit({ k: "msg", id: curMsgId, role: "assistant" });
		}
		const ame = pickObj(event, "assistantMessageEvent");
		const type = pickStr(ame, "type") ?? "";
		const delta = pickStr(ame, "delta") ?? pickStr(ame, "text") ?? "";
		if (delta === "") return;
		if (type.includes("thinking") || type.includes("reasoning")) {
			emit({ k: "think", id: curMsgId, delta });
		} else if (type.includes("text")) {
			emit({ k: "text", id: curMsgId, delta });
		}
	});

	pi.on("message_end", (event, ctx): void => {
		remember(ctx);
		const id = pickStr(event, "messageId") ?? pickStr(event, "id") ?? curMsgId;
		if (id !== "") emit({ k: "msgend", id });
		curMsgId = "";
	});

	pi.on("tool_execution_start", (event, ctx): void => {
		remember(ctx);
		const callId = pickStr(event, "toolCallId") ?? pickStr(event, "id") ?? `t${++msgSeq}`;
		const name = pickStr(event, "toolName") ?? pickStr(event, "name") ?? "tool";
		const input = pickObj(event, "input") ?? pickObj(event, "args");
		emit({ k: "tool", callId, name, input, status: "start" });
	});

	pi.on("tool_execution_end", (event, ctx): void => {
		remember(ctx);
		const callId = pickStr(event, "toolCallId") ?? pickStr(event, "id") ?? "";
		if (callId === "") return;
		const result = pickObj(event, "result");
		const ok = !isTruthy(pickObj(result, "isError")) && !isTruthy(pickObj(event, "isError"));
		emit({ k: "toolend", callId, result: summarizeResult(result), ok });
	});

	const status = (text: string): ((event: unknown, ctx: unknown) => void) => {
		return (_event, ctx): void => {
			remember(ctx);
			emit({ k: "status", text });
		};
	};
	pi.on("agent_start", status("working…"));
	pi.on("turn_start", status("working…"));
	pi.on("agent_end", status("idle"));
}
