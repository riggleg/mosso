# Queued messages

While Claude is working in a workspace, you can keep sending messages: they
are queued and sent one at a time, in order, each when Claude has finished
the turn before it. Every queued message continues the same Claude Code
session (`--resume`) with the workspace's model, exactly like a follow-up
sent by hand. This is not mid-task steering: a queued message never reaches
Claude while it is still working on the previous one.

## In the web UI

- The composer stays enabled while Claude works; its button says **Queue**.
- Queued messages appear at the end of the chat, marked **Queued** (or
  **Sending…** once Mosso is handing one to the sandbox), with a line saying
  when they go. Each has a **Cancel** button until it is being sent.
- When a queued message is sent it becomes an ordinary message in the
  conversation, followed by Claude's work on it.
- With Claude idle and nothing queued, **Send** works as before.

## When the queue waits

| What happened | The queue |
| --- | --- |
| Claude finished the turn | The next message is sent at once. |
| You clicked **Stop** | Paused at once, however the turn then ends: nothing more is sent until you click **Resume queue** (or cancel the messages). |
| The turn failed (Claude reported an error, or a queued message could not be sent) | Paused, so the rest does not run into the same problem. **Resume queue** sends the next one. |
| The sandbox session ended | Kept, not paused: it goes on as soon as the sandbox runs again (**Resume** in the header, **Resume queue**, or a new message, which joins the end of the queue and resumes the sandbox like any message sent to a stopped sandbox). |
| Mosso stopped or restarted | Kept in the database. After the restart Mosso catches up with the worker and goes on when Claude is done. |
| The sandbox is gone or the workspace terminated | Kept, shown as **Not sent**. |

While messages are paused and Claude is idle, a message you send goes at
once, as usual; it does not lift the pause. While they wait for Claude or
the sandbox, a new message joins the end of the queue so the order holds.

## Never twice

- Each queued message has an ID (`q_…`), which Mosso sends to the worker as
  the message's key. The worker runs a key once: a message sent again with
  the same key (after a restart, a lost answer or a retry) is answered with
  the task that already has it (see [worker.md](worker.md#api)).
- A queued message leaves the queue when the worker has it: when the worker
  accepts it or when its `user` event (which carries the key) arrives,
  whichever Mosso sees first. That change and the chat entry that replaces
  the queued message are saved in one transaction, so after a crash the
  message is either still queued or in the chat, never both or neither.
- Only one Claude turn runs per workspace: the queue uses the same
  per-workspace lock as messages and publishes. The synchronous
  `POST …/claude/messages` answers `409` while queued messages wait, so it
  cannot jump the queue.
- A message being sent can no longer be cancelled (`409`).

## Storage

The queue is saved in the workspace's `state` (`queue`, `queue_paused`;
see [history.md](history.md)), so no migration was needed. Each entry has
`id`, `text`, `attachments`, `time` and `sending`.

## API

| Method & path | Description |
| --- | --- |
| `POST /workspaces/{name}/chat` | As before; while Claude (or a publish) is busy, or earlier queued messages wait, the message is queued and saved instead: `202 {"status":"queued","id":"q_…"}`. `500` if it could not be saved (it is then not queued). |
| `DELETE /workspaces/{name}/queue/{id}` | Cancels a queued message; returns the workspace. `404` if it was sent or cancelled already; `409` while it is being sent. Works without Vercel. |
| `POST /workspaces/{name}/queue/resume` | Lifts a pause and sends the next message when the workspace is free, resuming a stopped sandbox; returns the workspace. `410` if the sandbox is gone. |

`GET /workspaces` and the event stream's `workspace` events include `queue`
(the queued messages, in order) and `queue_paused` (`"stopped"`, `"failed"`
or absent).
