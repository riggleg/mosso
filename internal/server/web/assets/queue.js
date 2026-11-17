// Queue helpers for the web UI: how a workspace's queued messages (sent
// while Claude was working) are shown, and whether a message sent now is
// queued. No DOM here, so it can be tested with Node (see
// webtest/queue.test.js).
"use strict";

const Queue = (() => {
  // gone says the workspace has no sandbox any more: its queued messages
  // will not be sent.
  function gone(ws) {
    return ws.sandbox_state === "gone" || ws.sandbox_state === "terminated";
  }

  // view describes the workspace's queue for the end of the chat, or
  // returns null if nothing is queued: the messages in order, each marked
  // Queued (or Sending… once it is on its way, when it can no longer be
  // cancelled), and a note saying when they go. canResume offers to send
  // them now, when they wait for the user or for the sandbox.
  function view(ws) {
    const queue = (ws && ws.queue) || [];
    if (!queue.length) return null;
    const ended = gone(ws);
    const n = queue.length;
    let note;
    let canResume = false;
    if (ended) {
      note = "Not sent: the sandbox no longer exists.";
    } else if (ws.queue_paused === "stopped") {
      note = "Paused because you stopped Claude. Resume to send " + (n === 1 ? "it" : "them in order") + ", or cancel.";
      canResume = true;
    } else if (ws.queue_paused === "failed") {
      note = "Paused because the last message failed. Resume to send " + (n === 1 ? "it" : "them in order") + ", or cancel.";
      canResume = true;
    } else if (ws.status === "stopped") {
      note = "Waiting for the sandbox: sent when it resumes.";
      canResume = true;
    } else {
      note = n === 1 ? "Sent when Claude finishes." : "Sent one at a time, in order, when Claude finishes.";
    }
    return {
      title: n + " queued " + (n === 1 ? "message" : "messages"),
      note,
      canResume,
      items: queue.map((q) => ({
        id: q.id,
        text: q.text,
        attachments: q.attachments || [],
        label: q.sending ? "Sending…" : ended ? "Not sent" : "Queued",
        canCancel: !q.sending && !ended,
      })),
    };
  }

  // queuesNext says a message sent now is queued rather than sent: Claude
  // (or a publish) is busy, or messages are queued, paused or not.
  function queuesNext(ws) {
    if (!ws || gone(ws)) return false;
    if (ws.status === "working") return true;
    return !!(ws.queue && ws.queue.length);
  }

  return { view, queuesNext };
})();

if (typeof module !== "undefined") module.exports = Queue;
