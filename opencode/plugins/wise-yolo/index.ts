// Wise Yolo — OpenCode V2 plugin (architecture §8).
//
// Registers the `permission.evaluate` hook, screens shell command batches by
// spawning `wiseyolo check` once per permission event, and maps the verdict
// onto the event's effect (strictness-only unless `grantFromAsk` is set).
// Fails safe to `options.onError` (default `ask`) on any outage.
//
// Depends only on `@opencode/plugin` (types) and Node's child_process/crypto.
import { Plugin } from "@opencode/plugin";
import { spawn, type ChildProcess } from "node:child_process";
import {
	commandHash,
	defaultOptions,
	mapEffect,
	outageMessage,
	parseContract,
	type CheckContract,
	type DecisionEvent,
	type Effect,
	type Options,
} from "./mapping.ts";

// Minimal structural types for the slice of the OpenCode plugin context and
// the `permission.evaluate` event this plugin uses (see V2 plugin docs,
// Permissions hook). Full types come with @opencode/plugin at the call site;
// keeping a small local surface makes the test harness possible.
export interface PluginContext {
	options: Record<string, unknown>;
	log: (text: string) => void;
	warn: (text: string) => void;
	permission: {
		hook(
			name: "evaluate",
			callback: (event: DecisionEvent) => Promise<void>,
		): Promise<{ dispose(): Promise<void> }>;
	};
}

export default Plugin.define({
	id: "wise-yolo",

	async setup(ctx) {
		const hookCtx = ctx as unknown as PluginContext;
		const options = resolveOptions(hookCtx.options);
		logger = { log: hookCtx.log, warn: hookCtx.warn };

		// Doctor at setup, asynchronous, best effort (architecture §8):
		// never blocks startup, warns at most once while loaded.
		void doctorBestEffort(options).catch(() => {
			warnOnce("wiseyolo: screening will fall back to ask");
		});

		// One registration; OpenCode disposes it on unload.
		await hookCtx.permission.hook("evaluate", async (event) => {
			if (event.action !== "shell") return;
			await screen(event, options);
		});
	},
});

/** Current plugin logger; wired from setup. */
let logger: { log(text: string): void; warn(text: string): void } = {
	log(text) {
		console.log(text);
	},
	warn(text) {
		console.warn(text);
	},
};

/** Replace the plugin logger (used by the test harness). Returns the old one. */
export function setLogger(
	next: { log?(text: string): void; warn?(text: string): void },
): { log(text: string): void; warn(text: string): void } {
	const old = logger;
	logger = {
		log: next.log ?? old.log,
		warn: next.warn ?? old.warn,
	};
	return old;
}

/** Warn at most once (the doctor hint is emitted a single time per load). */
let warned = false;
function warnOnce(text: string): void {
	if (warned) return;
	warned = true;
	logger.warn(text);
}

/** Normalise user-provided options onto defaults (strictness preserved). */
export function resolveOptions(raw: unknown): Options & typeof defaultOptions {
	const o = (typeof raw === "object" && raw !== null ? raw : {}) as Partial<Options>;
	const onError: Effect =
		o.onError === "allow" || o.onError === "deny" || o.onError === "ask" ? o.onError : defaultOptions.onError;
	const timeout: number =
		typeof o.timeoutMs === "number" && Number.isFinite(o.timeoutMs) && o.timeoutMs > 0
			? o.timeoutMs
			: defaultOptions.timeoutMs;
	return {
		executable: typeof o.executable === "string" && o.executable.length > 0 ? o.executable : defaultOptions.executable,
		timeoutMs: timeout,
		onError,
		grantFromAsk: o.grantFromAsk === true,
		logDecisions: o.logDecisions === true,
		backend: typeof o.backend === "string" && o.backend.length > 0 ? o.backend : undefined,
	};
}

// ---------------------------------------------------------------------------
// check invocation
// ---------------------------------------------------------------------------

/** Result of one `check` invocation. */
type CheckRun =
	| { ok: true; contract: CheckContract }
	| { ok: false; reason: string; detail: string };

/**
 * Screen one shell permission event. Exactly one spawn of `wiseyolo check`
 * per permission event; the batch travels on stdin; the child is killed after
 * `options.timeoutMs`. Exported for the test harness.
 */
export async function screen(
	event: DecisionEvent,
	options: Options & typeof defaultOptions,
): Promise<void> {
	const run = await runCheck(event.resources, options);
	if (!run.ok) {
		applyError(event, options, run.reason, run.detail);
		return;
	}
	const outcome = mapEffect(event, run.contract, options);
	if (outcome === "unusable") {
		applyError(event, options, "unusable contract", "check output did not match the JSON contract");
		return;
	}
	if (options.logDecisions) {
		// Never log raw commands — only the hash and verdict effects.
		logger.log(
			JSON.stringify({
				plugin: "wise-yolo",
				sessionID: event.sessionID,
				commandHash: `sha256:${commandHash(event.resources)}`,
				verdicts: run.contract.results.map((r) => r.verdict),
				aggregate: run.contract.aggregate.effect,
				wall_ms: run.contract.meta.wall_ms,
			}),
		);
	}
}

/** Apply the outage effect with a message naming the failure. */
function applyError(event: DecisionEvent, options: Options & typeof defaultOptions, reason: string, detail: string): void {
	event.effect = options.onError;
	event.message = outageMessage(reason, detail);
}

/**
 * Spawn `check` once, feed the batch on stdin, and return the parsed
 * contract or a structured outage: spawn failure, non-zero exit without
 * contract JSON, malformed JSON, timeout, or missing/unusable `meta`.
 * The default `onError` is `ask` for any of these (architecture §8).
 */
export function runCheck(
	resources: readonly string[],
	options: Options & typeof defaultOptions,
): Promise<CheckRun> {
	return new Promise((resolve) => {
		const args = ["check"];
		// The plugin does NOT choose a backend unless the user's options set
		// one: the CLI resolves the backend itself (env default jev).
		if (options.backend) args.push("--backend", options.backend);

		let settled = false;
		const done = (r: CheckRun): void => {
			if (settled) return;
			settled = true;
			clearTimeout(timer);
			resolve(r);
		};

		let child: ChildProcess;
		try {
			child = spawn(options.executable, args, { stdio: ["pipe", "pipe", "pipe"] });
		} catch (e) {
			done({ ok: false, reason: "spawn failed", detail: String(e) });
			return;
		}

		const timer = setTimeout(() => {
			child.kill();
			done({
				ok: false,
				reason: "timeout",
				detail: `did not finish within ${options.timeoutMs} ms (timeoutMs)`,
			});
		}, options.timeoutMs);

		child.on("error", (err: Error) => {
			// ENOENT and friends: the binary is missing or not executable.
			done({
				ok: false,
				reason: "spawn failed",
				detail: `could not run ${options.executable}: ${err.message}`,
			});
		});

		let stdout = "";
		let stderrText = "";
		child.stdout?.on("data", (chunk: { toString(): string }) => {
			stdout += chunk.toString();
		});
		child.stderr?.on("data", (chunk: { toString(): string }) => {
			stderrText += chunk.toString();
		});
		child.on("close", (code: number | null) => {
			if (code !== 0) {
				done({
					ok: false,
					reason: "non-zero exit",
					detail: `exit ${code}${stderrText ? `: ${stderrText.trim()}` : " (no stderr)"}`,
				});
				return;
			}
			let raw: unknown;
			try {
				raw = JSON.parse(stdout) as unknown;
			} catch (e) {
				done({
					ok: false,
					reason: "unusable contract",
					detail: `invalid JSON on stdout: ${e instanceof Error ? e.message : String(e)}`,
				});
				return;
			}
			const contract = parseContract(raw);
			if (contract === undefined) {
				done({
					ok: false,
					reason: "unusable contract",
					detail: stderrText.trim() || "check output did not match the JSON contract",
				});
				return;
			}
			done({ ok: true, contract });
		});

		child.stdin?.end(`${JSON.stringify({ commands: [...resources] })}\n`);
	});
}

// ---------------------------------------------------------------------------
// doctor at setup (best effort, never blocking)
// ---------------------------------------------------------------------------

/**
 * Run `doctor` asynchronously and log the result: healthy → model and
 * threshold facts; unhealthy → warn once that screening will fall back to
 * `ask`. Any failure resolves to a warning, never a throw into startup.
 */
async function doctorBestEffort(options: Options & typeof defaultOptions): Promise<void> {
	const args = ["doctor", "--json"]; // --json is accepted for the plugin contract
	if (options.backend) args.push("--backend", options.backend);
	const raw = await new Promise<string>((resolve) => {
		let child: ChildProcess;
		let out = "";
		try {
			child = spawn(options.executable, args, { stdio: ["ignore", "pipe", "ignore"] });
		} catch {
			resolve("");
			return;
		}
		child.on("error", () => {
			resolve("");
		});
		child.stdout?.on("data", (chunk: { toString(): string }) => {
			out += chunk.toString();
		});
		child.on("close", () => {
			resolve(out);
		});
		setTimeout(() => {
			child.kill();
			resolve(out);
		}, Math.min(options.timeoutMs, 5000));
	});
	const reports = parseDoctor(raw);
	if (reports === undefined || reports.length === 0) {
		warnOnce("wiseyolo: screening will fall back to ask");
		return;
	}
	// doctor's own verdict: exit 0 only when every reported backend is ok.
	const allOk = reports.every((r) => r.ok);
	if (!allOk) {
		// Unhealthy (e.g. a missing key on the default backend): warn once.
		warnOnce("wiseyolo: screening will fall back to ask");
		return;
	}
	const facts: string[] = [];
	for (const r of reports) {
		const model = typeof r.model === "string" ? r.model : "?";
		const policy = typeof r.policy_version === "string" ? r.policy_version : "?";
		const thresholds = typeof r.thresholds_version === "string" ? r.thresholds_version : "?";
		facts.push(`backend ${r.backend}: model ${model}, policy ${policy}, thresholds ${thresholds}`);
	}
	logger.log(`wiseyolo: ${facts.join("; ")}`);
}

interface DoctorReport {
	ok: boolean;
	backend: string;
	model?: string;
	policy_version?: string;
	thresholds_version?: string;
}

/** Parse `doctor` stdout: one JSON object per backend. */
function parseDoctor(raw: string): DoctorReport[] | undefined {
	const text = raw.trim();
	if (text.length === 0) return undefined;
	const reports: DoctorReport[] = [];
	// doctor emits one compact JSON object per backend; accept a single
	// object or whitespace/newline-separated objects.
	for (const line of text.split("\n")) {
		const trimmed = line.trim();
		if (trimmed.length === 0) continue;
		const value = safeJson(trimmed);
		if (value === undefined) return undefined;
		const v = value as Record<string, unknown>;
		if (typeof v["ok"] !== "boolean" || typeof v["backend"] !== "string") return undefined;
		reports.push({
			ok: v["ok"],
			backend: v["backend"],
			model: typeof v["model"] === "string" ? v["model"] : undefined,
			policy_version: typeof v["policy_version"] === "string" ? v["policy_version"] : undefined,
			thresholds_version: typeof v["thresholds_version"] === "string" ? v["thresholds_version"] : undefined,
		});
	}
	return reports;
}

function safeJson(text: string): unknown {
	try {
		return JSON.parse(text) as unknown;
	} catch {
		return undefined;
	}
}
