// Run with: node --test internal/server/webtest/*.test.js
"use strict";
const test = require("node:test");
const assert = require("node:assert");
const Lifecycle = require("../web/assets/lifecycle.js");

const hobby = { configured: true, plan: "hobby", plan_known: true, max_session_seconds: 2700 };
const pro = { configured: true, plan: "pro", plan_known: true, max_session_seconds: 86400 };

test("durations read naturally", () => {
  assert.strictEqual(Lifecycle.duration(900), "15 minutes");
  assert.strictEqual(Lifecycle.duration(2700), "45 minutes");
  assert.strictEqual(Lifecycle.duration(3600), "1 hour");
  assert.strictEqual(Lifecycle.duration(5400), "1 hour 30 minutes");
  assert.strictEqual(Lifecycle.duration(86400), "24 hours");
  assert.strictEqual(Lifecycle.duration(60), "1 minute");
});

test("Hobby offers sessions up to 45 minutes", () => {
  const values = Lifecycle.sessionOptions(hobby).map((o) => o.value);
  assert.deepStrictEqual(values, [900, 1800, 2700]);
});

test("Pro and Enterprise offer sessions up to 24 hours", () => {
  const values = Lifecycle.sessionOptions(pro).map((o) => o.value);
  assert.deepStrictEqual(values, [900, 1800, 2700, 3600, 7200, 14400, 28800, 43200, 86400]);
  const enterprise = { ...pro, plan: "enterprise" };
  assert.strictEqual(Lifecycle.sessionOptions(enterprise).length, 9);
});

test("an unknown plan, or no answer yet, gets the Hobby limits", () => {
  assert.deepStrictEqual(Lifecycle.sessionOptions(null).map((o) => o.value), [900, 1800, 2700]);
  assert.deepStrictEqual(Lifecycle.sessionOptions({ configured: true, plan: "", max_session_seconds: 2700 }).map((o) => o.value), [900, 1800, 2700]);
  assert.strictEqual(Lifecycle.planSummary({ configured: true, plan: "galaxy", plan_known: false, max_session_seconds: 2700 }), "galaxy · sessions up to 45 minutes (Hobby limits)");
  assert.strictEqual(Lifecycle.planSummary({ configured: true, plan: "", max_session_seconds: 2700 }), "plan unknown · sessions up to 45 minutes (Hobby limits)");
  assert.strictEqual(Lifecycle.planSummary({ configured: false }), "");
});

test("the default session is 45 minutes whatever the plan", () => {
  for (const plan of [hobby, pro]) {
    const def = Lifecycle.sessionOptions(plan).find((o) => o.value === Lifecycle.DEFAULT_SESSION);
    assert.ok(def && /default/.test(def.label));
  }
});

test("plan summaries", () => {
  assert.strictEqual(Lifecycle.planSummary(hobby), "Hobby · sessions up to 45 minutes");
  assert.strictEqual(Lifecycle.planSummary(pro), "Pro · sessions up to 24 hours");
});

test("idle timeouts offer 5, 15, 30, 60 minutes and never, 15 by default", () => {
  const opts = Lifecycle.idleOptions();
  assert.deepStrictEqual(opts.map((o) => o.value), [300, 900, 1800, 3600, 0]);
  assert.strictEqual(opts[4].label, "Never");
  assert.match(opts[1].label, /default/);
  assert.strictEqual(Lifecycle.DEFAULT_IDLE, 900);
  // A value set through the API is still shown.
  assert.deepStrictEqual(Lifecycle.idleOptions(7200).map((o) => o.value), [300, 900, 1800, 3600, 7200, 0]);
  assert.deepStrictEqual(Lifecycle.idleOptions(0).map((o) => o.value), [300, 900, 1800, 3600, 0]);
});

test("statuses", () => {
  const label = (status, extra) => Lifecycle.statusLabel({ status, ...extra });
  assert.strictEqual(label("ready"), "Idle");
  assert.strictEqual(label("working"), "Working");
  assert.strictEqual(label("creating"), "Setting up");
  assert.strictEqual(label("stopping"), "Stopping");
  assert.strictEqual(label("stopped"), "Stopped");
  assert.strictEqual(label("resuming"), "Resuming");
  assert.strictEqual(label("error"), "Error");
  assert.strictEqual(label("error", { sandbox_state: "gone" }), "Gone");
  assert.strictEqual(label("terminated"), "Terminated");
  assert.strictEqual(label("terminated", { archived_at: "2026-01-01T00:00:00Z" }), "Archived");
});

test("idle countdown", () => {
  const now = Date.parse("2026-11-17T10:00:00Z");
  assert.strictEqual(Lifecycle.idleText({ idle_stop_at: "2026-11-17T10:12:30Z" }, now), "Stops when idle in 13 min");
  assert.strictEqual(Lifecycle.idleText({ idle_stop_at: "2026-11-17T10:00:30Z" }, now), "Stops when idle in under a minute");
  // Nothing while Claude works, the sandbox is stopped or idle shutdown is off.
  assert.strictEqual(Lifecycle.idleText({ status: "working" }, now), "");
});

test("a workspace's settings, including ones from before they could be chosen", () => {
  assert.strictEqual(Lifecycle.settingsText({ session_seconds: 86400, idle_timeout_seconds: 900 }), "Sessions up to 24 hours, stops after 15 minutes idle");
  assert.strictEqual(Lifecycle.settingsText({ session_seconds: 2700, idle_timeout_seconds: 0 }), "Sessions up to 45 minutes, never stopped when idle");
});
