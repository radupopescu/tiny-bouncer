// Mapping of the wiseyolo `check` output contract onto an OpenCode permission
// decision (architecture §8). Pure logic only: deterministic, no I/O beyond
// the crypto hash. Everything here is unit-testable without spawning the
// classifier.
import { createHash } from "node:crypto";

/**
 * Permission effect, mirroring OpenCode's `allow` / `ask` / `deny`.
 */
export type Effect = "allow" | "ask" | "deny";

/**
 * Plugin options, read from `ctx.options` — the object form of the plugin
 * registration in `opencode.jsonc`. All fields optional; see defaultOptions.
 */
export interface Options {
	/** Binary path (absolute or on PATH). Default "wiseyolo". */
	executable?: string;
	/** Plugin-side kill timer in ms; must exceed the classifier's own budget. Default 20000. */
	timeoutMs?: number;
	/** Effect applied when the classifier cannot be consulted. Default "ask". */
	onError?: Effect;
	/**
	 * Allow a classifier `allow` to relax a configured `ask`. This is the only
	 * widening path in the plugin.
	 */
	grantFromAsk?: boolean;
	/** Log decisions (command hashes, never raw text) to the plugin log. Default false. */
	logDecisions?: boolean;
	/**
	 * Backend id passed to `wiseyolo check` as `--backend`. When unset the
	 * plugin passes no flag at all: the backend is resolved by the CLI's own
	 * environment convention (`WISE_YOLO_BACKEND`, default `jev`).
	 */
	backend?: string;
}

/** Defaults for every option field (architecture §8 options table). */
export const defaultOptions: Required<
	Pick<Options, "executable" | "timeoutMs" | "onError" | "grantFromAsk" | "logDecisions">
> = {
	executable: "wiseyolo",
	timeoutMs: 20_000,
	onError: "ask",
	grantFromAsk: false,
	logDecisions: false,
};

/** Per-command verdict inside the output contract (architecture §3). */
export interface CommandResult {
	command: string;
	verdict: Effect;
	confidence: number;
	categories?: string[];
	reason?: string;
}

/** Aggregate the plugin applies (architecture §3). */
export interface Aggregate {
	effect: Effect;
	reason: string;
}

/** The `check` output contract, as far as the plugin consumes it. */
export interface CheckContract {
	results: CommandResult[];
	aggregate: Aggregate;
	meta: Record<string, unknown> & { wall_ms: number };
}

/** The parts of an OpenCode permission evaluation the mapping touches. */
export interface DecisionEvent {
	sessionID: string;
	action: string;
	resources: readonly string[];
	effect: Effect;
	message?: string;
}

/** Outcome of applying the contract to an event. */
export type MapOutcome = "applied" | "unusable";

const EFFECTS: readonly Effect[] = ["allow", "ask", "deny"];

function isEffect(value: unknown): value is Effect {
	return typeof value === "string" && (EFFECTS as readonly string[]).includes(value);
}

/**
 * Validate a parsed `check` stdout as the output contract (architecture §3).
 * `meta` must exist and carry a finite, non-negative `wall_ms` number; the
 * aggregate must carry a known effect. Anything else is an outage and the
 * caller applies `onError`.
 */
export function parseContract(raw: unknown): CheckContract | undefined {
	if (typeof raw !== "object" || raw === null) return undefined;
	const o = raw as Record<string, unknown>;
	if (!Array.isArray(o["results"])) return undefined;
	const aggregate = o["aggregate"];
	if (typeof aggregate !== "object" || aggregate === null) return undefined;
	const agg = aggregate as Record<string, unknown>;
	if (!isEffect(agg["effect"]) || typeof agg["reason"] !== "string") return undefined;
	const results: CommandResult[] = [];
	for (const entry of o["results"]) {
		if (typeof entry !== "object" || entry === null) return undefined;
		const e = entry as Record<string, unknown>;
		if (typeof e["command"] !== "string" || !isEffect(e["verdict"])) return undefined;
		results.push({
			command: e["command"],
			verdict: e["verdict"],
			confidence: typeof e["confidence"] === "number" ? e["confidence"] : 0,
			categories: Array.isArray(e["categories"])
				? (e["categories"] as unknown[]).filter((c): c is string => typeof c === "string")
				: undefined,
			reason: typeof e["reason"] === "string" ? e["reason"] : undefined,
		});
	}
	const meta = o["meta"];
	if (typeof meta !== "object" || meta === null) return undefined;
	const m = meta as Record<string, unknown>;
	if (typeof m["wall_ms"] !== "number" || !Number.isFinite(m["wall_ms"]) || m["wall_ms"] < 0) {
		return undefined;
	}
	return { results, aggregate: { effect: agg["effect"], reason: agg["reason"] }, meta: m as CheckContract["meta"] };
}

/**
 * Apply the classifier aggregate onto the permission event.
 *
 * Strictness semantics (architecture §1, §8):
 * - classifier `deny` → `deny`, never softened to ask or allow;
 * - classifier `ask` → `ask`, never softened to allow;
 * - classifier `allow` → the event's configured effect is left untouched,
 *   unless `options.grantFromAsk` and the configured effect is `ask` — the
 *   only widening path, used only when the classifier says allow.
 *
 * The aggregate reason becomes the event message so the prompt or denial
 * carries the classifier's one-sentence explanation.
 *
 * Returns "unusable" when the contract shape is not a usable contract; the
 * caller must then apply `onError` instead.
 */
export function mapEffect(
	event: DecisionEvent,
	contract: CheckContract | undefined,
	options: Options,
): MapOutcome {
	if (contract === undefined) return "unusable";
	// A configured deny never reaches the hook (policies always win), but
	// guard defensively: the mapping never widens or softens a deny.
	if (event.effect === "deny") return "applied";
	switch (contract.aggregate.effect) {
		case "deny":
			event.effect = "deny";
			event.message = contract.aggregate.reason;
			return "applied";
		case "ask":
			event.effect = "ask";
			event.message = contract.aggregate.reason;
			return "applied";
		case "allow":
			if (options.grantFromAsk === true && event.effect === "ask") {
				event.effect = "allow";
				event.message = contract.aggregate.reason;
			}
			return "applied";
		default:
			return "unusable";
	}
}

/**
 * Human-readable message naming the outage, for the `onError` path.
 */
export function outageMessage(what: string, detail: string): string {
	return `wiseyolo: classifier unavailable (${what}): ${detail} — falling back to the configured effect`;
}

/**
 * sha256 of the joined resources, for decision logging. Resources are joined
 * with "\n" before hashing so the batch travels as one digest. The raw
 * command text is never logged (architecture §9); only this hash and the
 * verdicts travel to the log.
 */
export function commandHash(resources: readonly string[]): string {
	return createHash("sha256").update(resources.join("\n"), "utf8").digest("hex");
}
