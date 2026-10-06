// Wise Yolo plugin test harness. Runs the mapEffect unit table and the spawn
// end-to-end cases against the built `wiseyolo` binary with the mock backend.
// No network anywhere: the mock backend is fully offline.
//
// Run: npm test  (node --experimental-strip-types test/run.ts)
// Prerequisite: the host build must exist first —
//   make build   # from the repository root → bin/wiseyolo
import assert from "node:assert/strict";
import { mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import {
	commandHash,
	defaultOptions,
	mapEffect,
	parseContract,
	type CheckContract,
	type DecisionEvent,
	type Options,
} from "../mapping.ts";
import { doctorArgs, resolveOptions, screen, selectedBackend, setLogger } from "../index.ts";

const failures: string[] = [];
let passed = 0;
async function case_(name: string, fn: () => void | Promise<void>): Promise<void> {
	try {
		await fn();
		passed += 1;
		console.log(`ok   ${name}`);
	} catch (e) {
		failures.push(name);
		console.log(`FAIL ${name}\n${String(e instanceof Error ? e.message : e)}`);
	}
}

// Keep cache use off and redirected for this test process.
const cacheRoot = mkdtempSync(join(tmpdir(), "wise-yolo-plugin-test-"));
process.env["WISE_YOLO_CACHE_DIR"] = cacheRoot;
process.env["WISE_YOLO_CACHE"] = "false";

// --- mapEffect unit table ---------------------------------------------------

function ev(effect: DecisionEvent["effect"] = "allow"): DecisionEvent {
	return { sessionID: "s1", action: "shell", resources: ["git status"], effect };
}

function contract(effect: "allow" | "ask" | "deny", reason = "why"): CheckContract {
	return {
		results: [{ command: "git status", verdict: effect, confidence: 0.9, reason }],
		aggregate: { effect, reason },
		meta: { wall_ms: 10 },
	};
}

const base = defaultOptions as Options & typeof defaultOptions;

await case_("mapEffect: classifier deny denies a configured allow", () => {
	const e = ev("allow");
	assert.equal(mapEffect(e, contract("deny", "no rule matched"), base), "applied");
	assert.equal(e.effect, "deny");
	assert.equal(e.message, "no rule matched");
});

await case_("mapEffect: classifier deny denies a configured ask", () => {
	const e = ev("ask");
	mapEffect(e, contract("deny"), base);
	assert.equal(e.effect, "deny");
});

await case_("mapEffect: classifier deny stays deny even with grantFromAsk", () => {
	const e = ev("ask");
	mapEffect(e, contract("deny"), { ...base, grantFromAsk: true });
	assert.equal(e.effect, "deny");
});

await case_("mapEffect: classifier ask raises a configured allow to ask", () => {
	const e = ev("allow");
	mapEffect(e, contract("ask", "borderline"), base);
	assert.equal(e.effect, "ask");
	assert.equal(e.message, "borderline");
});

await case_("mapEffect: classifier ask keeps a configured ask", () => {
	const e = ev("ask");
	mapEffect(e, contract("ask"), base);
	assert.equal(e.effect, "ask");
});

await case_("mapEffect: classifier ask never softens a configured deny", () => {
	const e = ev("deny");
	mapEffect(e, contract("ask"), base);
	assert.equal(e.effect, "deny");
});

await case_("mapEffect: classifier allow leaves a configured allow untouched", () => {
	const e = ev("allow");
	mapEffect(e, contract("allow"), base);
	assert.equal(e.effect, "allow");
});

await case_("mapEffect: classifier allow leaves a configured ask untouched by default", () => {
	const e = ev("ask");
	mapEffect(e, contract("allow", "read-only"), base);
	assert.equal(e.effect, "ask");
	assert.equal(e.message, undefined, "no message is attached when untouched");
});

await case_("mapEffect: grantFromAsk relaxes a configured ask when the classifier allows", () => {
	const e = ev("ask");
	mapEffect(e, contract("allow", "read-only"), { ...base, grantFromAsk: true });
	assert.equal(e.effect, "allow");
	assert.equal(e.message, "read-only");
});

await case_("mapEffect: grantFromAsk only fires for a classifier allow", () => {
	// grantFromAsk must never widen on a classifier ask or deny.
	const d = ev("ask");
	mapEffect(d, contract("deny"), { ...base, grantFromAsk: true });
	assert.equal(d.effect, "deny");
	const a = ev("ask");
	mapEffect(a, contract("ask"), { ...base, grantFromAsk: true });
	assert.equal(a.effect, "ask");
});

await case_("mapEffect: missing contract is unusable (onError path)", () => {
	const e = ev("allow");
	assert.equal(mapEffect(e, undefined, base), "unusable");
	assert.equal(e.effect, "allow", "the mapping does not mutate the event; the caller applies onError");
});

await case_("parseContract: rejects malformed shapes", () => {
	assert.equal(parseContract(undefined), undefined);
	assert.equal(parseContract({}), undefined);
	assert.equal(parseContract({ results: [], aggregate: { effect: "banana", reason: "" } }), undefined);
	assert.equal(parseContract({ results: [], aggregate: { effect: "allow", reason: "" } }), undefined, "meta required");
	assert.equal(
		parseContract({ results: [], aggregate: { effect: "allow", reason: "" }, meta: { wall_ms: "x" } }),
		undefined,
		"wall_ms must be a number",
	);
	assert.equal(
		parseContract({
			results: [{ verdict: "allow" }],
			aggregate: { effect: "allow", reason: "" },
			meta: { wall_ms: 1 },
		}),
		undefined,
		"per-result verdict effect must be a known effect",
	);
});

await case_("parseContract: accepts the real shape", () => {
	const c = parseContract({
		results: [{ command: "git status", verdict: "allow", confidence: 0.97, categories: ["vcs_read"], reason: "r" }],
		aggregate: { effect: "allow", reason: "safe" },
		meta: { backend: "mock", backend_model: "mock-rules", wall_ms: 3, cached: false, attempts: "1" },
	});
	assert.ok(c);
	assert.equal(c.aggregate.effect, "allow");
	assert.equal(c.meta.wall_ms, 3);
});

await case_("commandHash: deterministic, input-sensitive, raw text never derivable", () => {
	assert.equal(commandHash(["git status"]), commandHash(["git status"]));
	assert.notEqual(commandHash(["git status"]), commandHash(["git status", "ls"]));
	assert.match(commandHash(["git status"]), /^[0-9a-f]{64}$/);
});

// --- spawn end-to-end (built binary, mock backend, no network) ---------------

const bin = resolve(import.meta.dirname, "../../../../bin/wiseyolo");

async function runEvent(
	resources: readonly string[],
	starting: DecisionEvent["effect"],
	overrides: Partial<Options> = {},
): Promise<DecisionEvent> {
	const event: DecisionEvent = {
		sessionID: "sess-test",
		action: "shell",
		resources: [...resources],
		effect: starting,
	};
	await screen(event, resolveOptions({ executable: bin, ...overrides }));
	return event;
}

await case_("e2e: dangerous batch → deny with the aggregate reason", async () => {
	const e = await runEvent(["git status", "rm -rf /"], "allow", { backend: "mock" });
	assert.equal(e.effect, "deny");
	assert.ok(typeof e.message === "string" && e.message.length > 0, "the denial carries a reason");
});

await case_("e2e: safe batch → configured effects untouched", async () => {
	const e = await runEvent(["git status", "ls -l"], "allow", { backend: "mock" });
	assert.equal(e.effect, "allow");
	const e2 = await runEvent(["git status", "ls -l"], "ask", { backend: "mock" });
	assert.equal(e2.effect, "ask", "a configured ask survives without grantFromAsk");
});

await case_("e2e: safe batch + grantFromAsk → a configured ask relaxes to allow", async () => {
	const e = await runEvent(["git status", "ls -l"], "ask", { backend: "mock", grantFromAsk: true });
	assert.equal(e.effect, "allow");
});

await case_("e2e: missing binary → onError (default ask) with an outage message", async () => {
	const e = await runEvent(["git status"], "allow", { executable: "/does/not/exist/wiseyolo" });
	assert.equal(e.effect, "ask");
	assert.match(e.message ?? "", /spawn failed/, "the message names the outage");
});

await case_("e2e: non-zero exit without contract JSON → onError with reason", async () => {
	// Unknown backend exits 1 before any contract is printed.
	const e = await runEvent(["git status"], "allow", { backend: "no-such-backend" });
	assert.equal(e.effect, "ask");
	assert.match(e.message ?? "", /non-zero exit/, "the message names the outage");
});

await case_("e2e: empty batch → the contract's allow leaves a configured ask untouched", async () => {
	const e = await runEvent([], "ask", { backend: "mock" });
	assert.equal(e.effect, "ask");
});

await case_("e2e: logDecisions logs hash + verdicts, never raw commands", async () => {
	const lines: string[] = [];
	const restore = setLogger({
		log: (text: string) => {
			lines.push(text);
		},
	});
	const secret = "SOME-VERY-DISTINCTIVE-SECRET-84710";
	try {
		await runEvent(["rm -rf /", "ls -l"], "ask", { backend: "mock", logDecisions: true });
	} finally {
		setLogger(restore);
	}
	const blob = lines.join("\n");
	assert.ok(blob.length > 0, "a decision line was logged");
	const entry = JSON.parse(blob) as Record<string, unknown>;
	assert.equal(entry["sessionID"], "sess-test");
	assert.match(String(entry["commandHash"]), /^sha256:[0-9a-f]{64}$/);
	assert.ok(Array.isArray(entry["verdicts"]) && entry["verdicts"].length === 2);
	assert.equal(entry["aggregate"], "deny");
	assert.equal(typeof entry["wall_ms"], "number");
	assert.equal(blob.includes(secret), false, "no raw command text ever reaches the log");
});

await case_("e2e: exit 0 with invalid JSON → onError path", async () => {
	// A stub executable that exits 0 printing garbage exercises the
	// JSON-parse-failure outage without any network or Go work.
	const stubDir = mkdtempSync(join(tmpdir(), "wise-yolo-stub-"));
	const stubPath = join(stubDir, "wiseyolo-stub.sh");
	writeFileSync(
		stubPath,
		"#!/bin/sh\necho 'not json at all'\n",
		{ mode: 0o755 },
	);
	const e = await runEvent(["git status"], "allow", { executable: stubPath });
	assert.equal(e.effect, "ask");
	assert.match(e.message ?? "", /invalid JSON/, "the message names the outage");
});

await case_("doctor is scoped to the selected backend", () => {
	// An explicit plugin option wins over the environment; the environment
	// wins over the CLI default. This keeps an unused optional backend
	// (api, afm) from warning at setup.
	assert.deepEqual(doctorArgs({ backend: "api" }), ["doctor", "--json", "--backend", "api"]);
	assert.equal(selectedBackend({}, { WISE_YOLO_BACKEND: "afm" }), "afm");
	assert.equal(selectedBackend({ backend: "api" }, { WISE_YOLO_BACKEND: "afm" }), "api");
	assert.equal(selectedBackend({}, {}), "jev");
});

if (failures.length > 0) {
	console.error(`\n${failures.length} failing case(s): ${failures.join(", ")}`);
	process.exit(1);
}
console.log(`\nall ${passed} plugin tests passed`);
