// Run with: node --test internal/server/webtest/*.test.js
"use strict";
const test = require("node:test");
const assert = require("node:assert");
const Queue = require("../web/assets/queue.js");

const q = (id, text, extra) => ({ id, text, time: "2026-11-17T10:00:00Z", ...extra });

test("nothing queued shows nothing", () => {
  assert.strictEqual(Queue.view({ status: "working" }), null);
  assert.strictEqual(Queue.view({ status: "working", queue: [] }), null);
  assert.strictEqual(Queue.view(null), null);
});

test("queued messages are listed in order and marked Queued", () => {
  const v = Queue.view({ status: "working", queue: [q("q_1", "one"), q("q_2", "two"), q("q_3", "three")] });
  assert.strictEqual(v.title, "3 queued messages");
  assert.deepStrictEqual(
    v.items.map((i) => [i.id, i.text, i.label, i.canCancel]),
    [
      ["q_1", "one", "Queued", true],
      ["q_2", "two", "Queued", true],
      ["q_3", "three", "Queued", true],
    ],
  );
  assert.match(v.note, /in order, when Claude finishes/);
  assert.strictEqual(v.canResume, false);
});

test("a message being sent can no longer be cancelled", () => {
  const v = Queue.view({ status: "working", queue: [q("q_1", "one", { sending: true }), q("q_2", "two")] });
  assert.deepStrictEqual(
    v.items.map((i) => [i.label, i.canCancel]),
    [
      ["Sending…", false],
      ["Queued", true],
    ],
  );
});

test("a paused queue says why and offers to resume", () => {
  const stopped = Queue.view({ status: "ready", queue_paused: "stopped", queue: [q("q_1", "one")] });
  assert.match(stopped.note, /you stopped Claude/);
  assert.strictEqual(stopped.canResume, true);
  assert.strictEqual(stopped.title, "1 queued message");
  const failed = Queue.view({ status: "error", queue_paused: "failed", queue: [q("q_1", "one"), q("q_2", "two")] });
  assert.match(failed.note, /last message failed.*in order/);
  assert.strictEqual(failed.canResume, true);
});

test("a queue waiting for a stopped sandbox can resume it", () => {
  const v = Queue.view({ status: "stopped", sandbox_state: "stopped", queue: [q("q_1", "one")] });
  assert.match(v.note, /Waiting for the sandbox/);
  assert.strictEqual(v.canResume, true);
});

test("without a sandbox, queued messages are shown as not sent", () => {
  for (const ws of [
    { status: "terminated", sandbox_state: "terminated" },
    { status: "error", sandbox_state: "gone" },
  ]) {
    const v = Queue.view({ ...ws, queue: [q("q_1", "one")] });
    assert.strictEqual(v.items[0].label, "Not sent");
    assert.strictEqual(v.items[0].canCancel, false);
    assert.strictEqual(v.canResume, false);
  }
});

test("a message is queued while Claude works or while others wait", () => {
  assert.strictEqual(Queue.queuesNext({ status: "working" }), true);
  assert.strictEqual(Queue.queuesNext({ status: "ready" }), false);
  assert.strictEqual(Queue.queuesNext({ status: "stopped", queue: [q("q_1", "one")] }), true);
  // Paused: a message sent while Claude is idle goes at once.
  assert.strictEqual(Queue.queuesNext({ status: "ready", queue_paused: "failed", queue: [q("q_1", "one")] }), false);
  assert.strictEqual(Queue.queuesNext({ status: "error", sandbox_state: "gone", queue: [q("q_1", "one")] }), false);
  assert.strictEqual(Queue.queuesNext(null), false);
});
