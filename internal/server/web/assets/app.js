// Mosso's web UI. Plain JavaScript, no build step. It talks to the local Go
// server's JSON API and follows each workspace's chat over server-sent
// events. Everything Claude produces is untrusted text: it is only ever put
// into the page with textContent (or the DOM-building Markdown renderer).
"use strict";

const $ = (id) => document.getElementById(id);

const state = {
  workspaces: [],
  listError: null,
  selected: null, // workspace name
  ws: null, // latest snapshot of the selected workspace
  source: null, // EventSource for the selected workspace
  entries: [], // the selected workspace's chat log
  tools: new Map(), // tool_use_id -> {details, state, body, children}
  drafts: new Map(), // workspace name -> unsent composer text
  sending: false,
  publishing: false,
  repos: null,
};

// ---------- helpers ----------

function h(tag, props, ...children) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k === "class") e.className = v;
    else if (k === "text") e.textContent = v;
    else if (k.startsWith("on")) e.addEventListener(k.slice(2), v);
    else e.setAttribute(k, v === true ? "" : v);
  }
  for (const c of children) {
    if (c === null || c === undefined || c === false) continue;
    e.appendChild(typeof c === "string" ? document.createTextNode(c) : c);
  }
  return e;
}

class APIError extends Error {
  constructor(status, message) {
    super(message);
    this.status = status;
  }
}

async function api(method, path, body) {
  const opts = { method, headers: {} };
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  let res;
  try {
    res = await fetch(path, opts);
  } catch (e) {
    throw new APIError(0, "Cannot reach the Mosso server. Is it still running?");
  }
  if (res.status === 204) return null;
  let data = null;
  try {
    data = await res.json();
  } catch (e) {
    /* not JSON */
  }
  if (!res.ok) throw new APIError(res.status, (data && data.error) || `HTTP ${res.status}`);
  return data;
}

const wsPath = (name) => "/workspaces/" + encodeURIComponent(name);

function showBanner(msg) {
  $("banner-text").textContent = msg;
  $("banner").classList.remove("hidden");
}
function hideBanner() {
  $("banner").classList.add("hidden");
}

function fmtDuration(ms) {
  const s = Math.max(0, Math.round(ms / 1000));
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${String(s % 60).padStart(2, "0")}s`;
  return `${Math.floor(m / 60)}h ${m % 60}m`;
}

function shortRepo(repo) {
  return repo.slice(repo.indexOf("/") + 1);
}

function firstLine(text, max) {
  const line = (text || "").trim().split("\n")[0].trim();
  return line.length > max ? line.slice(0, max - 1) + "…" : line;
}

const authHint = /\b(401|403)\b|auth|log ?in|oauth|token|credential|unauthori[sz]ed/i;

// ---------- configuration checks ----------

async function loadChecks() {
  const list = $("checks");
  const item = (label, ok, detail) =>
    h("li", {}, h("span", { class: ok ? "check-ok" : "check-bad", text: ok ? "●" : "○" }), h("span", { text: label }), h("span", { class: "check-detail", title: detail || "", text: detail || "" }));
  const [claude, github] = await Promise.all([api("GET", "/claude/auth").catch((e) => ({ error: e.message })), api("GET", "/github/auth").catch((e) => ({ error: e.message }))]);
  list.replaceChildren(
    item("Vercel", !(state.listError && state.listError.status === 503), state.listError && state.listError.status === 503 ? "not configured" : ""),
    item("Claude", claude.configured, claude.configured ? claude.source : "run mosso auth claude"),
    item("GitHub", github.configured && !github.error, !github.configured ? "run mosso auth github" : github.login ? "@" + github.login : "token problem"),
  );
  if (github.error && github.configured) list.lastChild.title = github.error;
}

// ---------- workspace list ----------

async function refreshList() {
  try {
    const data = await api("GET", "/workspaces");
    state.workspaces = data.workspaces;
    const hadError = state.listError;
    state.listError = null;
    if (hadError) loadChecks();
  } catch (e) {
    state.listError = e;
    state.workspaces = [];
  }
  if (state.selected && !state.workspaces.some((w) => w.name === state.selected) && !state.listError) {
    select(null);
  } else if (!state.selected && state.workspaces.length) {
    const fromHash = decodeURIComponent(location.hash.replace(/^#\/?/, ""));
    const pick = state.workspaces.find((w) => w.name === fromHash) || state.workspaces[state.workspaces.length - 1];
    select(pick.name);
  }
  const cur = state.workspaces.find((w) => w.name === state.selected);
  if (cur) state.ws = cur;
  render();
}

function renderSidebar() {
  const nav = $("workspace-list");
  if (state.listError) {
    nav.replaceChildren(h("div", { class: "list-empty error-text", text: state.listError.message }));
    return;
  }
  if (!state.workspaces.length) {
    nav.replaceChildren(h("div", { class: "list-empty", text: "No workspaces yet." }));
    return;
  }
  nav.replaceChildren(
    ...state.workspaces.map((w) =>
      h(
        "button",
        { type: "button", class: "ws-item" + (w.name === state.selected ? " selected" : ""), title: `${w.repo} (${w.name})`, onclick: () => select(w.name) },
        h("div", { class: "ws-item-top" }, h("span", { class: "dot " + w.status, title: w.status }), h("span", { class: "ws-item-repo", text: shortRepo(w.repo) })),
        h("div", { class: "ws-item-sub", text: `${w.base} → ${w.branch.replace(/^mosso\//, "")}` }),
      ),
    ),
  );
}

function statusLabel(ws) {
  switch (ws.status) {
    case "creating":
      return "Setting up";
    case "working":
      return "Working";
    case "error":
      return "Error";
    default:
      return "Ready";
  }
}

function renderHeader() {
  const ws = state.ws;
  if (!ws) return;
  const repo = $("ws-repo");
  repo.textContent = ws.repo;
  repo.href = "https://github.com/" + ws.repo;
  $("ws-branch").textContent = `${ws.base} → ${ws.branch}`;
  $("ws-branch").title = "Base branch → working branch";
  const status = $("ws-status");
  status.textContent = statusLabel(ws);
  status.className = "status " + ws.status;

  const pr = $("ws-pr");
  if (ws.pull_request) {
    pr.textContent = `PR #${ws.pull_request.number} ↗`;
    pr.href = ws.pull_request.url;
    pr.classList.remove("hidden");
  } else {
    pr.classList.add("hidden");
  }
  const busy = ws.status === "working" || ws.status === "creating";
  const publish = $("publish");
  publish.textContent = state.publishing ? "Publishing…" : ws.pull_request ? "Update PR" : "Create PR";
  publish.disabled = busy || state.publishing;
  publish.title = busy ? "Wait for the current task to finish" : "Commit and push the sandbox's changes as a pull request";

  const err = $("ws-error");
  if (ws.error && ws.status === "error") {
    err.textContent = ws.error;
    err.classList.remove("hidden");
  } else {
    err.classList.add("hidden");
  }
  renderExpiry();
}

function renderExpiry() {
  const ws = state.ws;
  const el = $("ws-expiry");
  if (!ws || !ws.expires_at) return;
  const left = new Date(ws.expires_at).getTime() - Date.now();
  if (left <= 0) {
    el.textContent = "Sandbox expired";
    el.className = "soon";
  } else {
    el.textContent = "Sandbox expires in " + (left < 60000 ? "under a minute" : Math.floor(left / 60000) + " min");
    el.className = left < 5 * 60000 ? "soon" : "muted";
  }
  el.title = "Vercel stops the sandbox at " + new Date(ws.expires_at).toLocaleTimeString() + ". Publish before then.";
}

function isWorking() {
  return state.ws && (state.ws.status === "working" || state.ws.status === "creating");
}

function lastEntryOf(types) {
  for (let i = state.entries.length - 1; i >= 0; i--) if (types.includes(state.entries[i].type)) return state.entries[i];
  return null;
}

function claudeRunning() {
  // A turn is running if the latest prompt has not been followed by its end.
  const last = lastEntryOf(["user", "result", "error", "pull_request", "notice"]);
  if (!last) return false;
  if (last.type === "user") return true;
  return last.type === "notice" && last.text.startsWith("Installing");
}

function renderWorking() {
  const ws = state.ws;
  const working = $("working");
  const busy = isWorking();
  working.classList.toggle("hidden", !busy);
  if (busy) {
    const running = claudeRunning();
    let text = "Working…";
    let since = null;
    if (ws.status === "creating") text = "Setting up the workspace: provisioning the sandbox, cloning and installing Claude Code…";
    else if (running) {
      text = "Claude is working…";
      since = lastEntryOf(["user"]);
    } else if (state.publishing || (lastEntryOf(["notice"]) || {}).text?.startsWith("Publishing")) {
      text = "Publishing to GitHub…";
      since = lastEntryOf(["notice"]);
    }
    $("working-text").textContent = text;
    $("working-time").textContent = since ? fmtDuration(Date.now() - new Date(since.time).getTime()) : "";
    $("stop").classList.toggle("hidden", !running);
  }

  const prompt = $("prompt");
  const send = $("send");
  const hint = $("composer-hint");
  const blocked = busy || state.sending || !ws;
  send.disabled = blocked;
  if (ws && ws.status === "creating") {
    prompt.placeholder = "The workspace is being set up…";
    hint.textContent = "";
  } else if (busy) {
    prompt.placeholder = "Claude is busy. You can type your next message; send it when the current task finishes.";
    hint.textContent = "Busy";
  } else {
    prompt.placeholder = "Ask Claude to make a change…  (Enter to send, Shift+Enter for a new line)";
    hint.textContent = state.entries.some((e) => e.type === "user") ? "Follow-ups continue the same conversation." : "";
  }
}

function render() {
  renderSidebar();
  const hasWs = !!(state.selected && state.ws);
  $("workspace").classList.toggle("hidden", !hasWs);
  $("empty").classList.toggle("hidden", hasWs);
  const emptyErr = $("empty-error");
  if (state.listError) {
    emptyErr.textContent = state.listError.message;
    emptyErr.classList.remove("hidden");
  } else {
    emptyErr.classList.add("hidden");
  }
  $("empty-new").disabled = !!state.listError;
  $("new-workspace").disabled = !!state.listError;
  if (hasWs) {
    renderHeader();
    renderWorking();
    $("chat-empty").classList.toggle("hidden", state.entries.length > 0 || state.ws.status === "creating");
  }
}

// ---------- selecting a workspace and following its chat ----------

function select(name) {
  if (name === state.selected && state.source) return;
  if (state.selected) state.drafts.set(state.selected, $("prompt").value);
  if (state.source) state.source.close();
  state.source = null;
  state.selected = name;
  state.ws = state.workspaces.find((w) => w.name === name) || null;
  state.entries = [];
  state.tools = new Map();
  state.publishing = false;
  $("entries").replaceChildren();
  $("prompt").value = (name && state.drafts.get(name)) || "";
  history.replaceState(null, "", name ? "#" + encodeURIComponent(name) : location.pathname);
  hideBanner();
  if (name) {
    follow(name);
    setTimeout(() => $("prompt").focus(), 0);
  }
  render();
}

function follow(name) {
  const es = new EventSource(wsPath(name) + "/events");
  state.source = es;
  es.addEventListener("entry", (ev) => {
    if (state.source !== es) return;
    const entry = JSON.parse(ev.data);
    if (entry.id < state.entries.length) return; // already shown
    const chat = $("chat");
    const atBottom = chat.scrollHeight - chat.scrollTop - chat.clientHeight < 80;
    state.entries.push(entry);
    renderEntry(entry);
    render();
    if (atBottom) chat.scrollTop = chat.scrollHeight;
  });
  es.addEventListener("workspace", (ev) => {
    if (state.source !== es) return;
    const ws = JSON.parse(ev.data);
    state.ws = ws;
    const i = state.workspaces.findIndex((w) => w.name === ws.name);
    if (i >= 0) state.workspaces[i] = ws;
    render();
  });
  es.addEventListener("closed", () => {
    es.close();
    if (state.source === es) refreshList();
  });
  es.onopen = () => {
    if (state.source === es) hideBanner();
  };
  es.onerror = () => {
    if (state.source !== es) return;
    if (es.readyState === EventSource.CLOSED) {
      refreshList(); // the workspace is gone
    } else {
      showBanner("Lost the connection to Mosso; reconnecting… A running task keeps going on the server.");
    }
  };
}

// ---------- rendering chat entries ----------

function renderEntry(e) {
  const box = $("entries");
  switch (e.type) {
    case "user":
      box.appendChild(h("div", { class: "entry user-msg" }, h("span", { class: "who", text: "You" }), e.text));
      break;
    case "claude":
      renderEvent(e.event);
      break;
    case "result": {
      const r = e.result;
      if (r.is_error) {
        box.appendChild(errorBox(r.text || "Claude Code reported an error (" + (r.subtype || "unknown") + ").", r.text));
      }
      const bits = [r.is_error ? "Turn failed" : "Done", fmtDuration(r.duration_ms)];
      if (r.num_turns) bits.push(r.num_turns + (r.num_turns === 1 ? " step" : " steps"));
      box.appendChild(h("div", { class: "entry turn-end", text: bits.join(" · ") }));
      markPendingTools();
      break;
    }
    case "error":
      box.appendChild(errorBox(e.text, e.text));
      box.appendChild(h("div", { class: "entry turn-end", text: "Stopped with an error" }));
      markPendingTools();
      break;
    case "notice":
      box.appendChild(h("div", { class: "entry notice", text: e.text }));
      if (e.text.startsWith("Stopped")) markPendingTools();
      break;
    case "pull_request": {
      const pr = e.pull_request;
      box.appendChild(
        h(
          "div",
          { class: "entry pr-card" },
          h("strong", { text: pr.created ? "Opened pull request" : "Updated pull request" }),
          h("a", { href: pr.url, target: "_blank", rel: "noopener noreferrer", text: `#${pr.number} ↗` }),
          h("span", { class: "muted", text: `${pr.branch} @ ${pr.commit.slice(0, 7)}` }),
        ),
      );
      break;
    }
  }
}

function errorBox(text, forHint) {
  const box = h("div", { class: "entry error-box", text });
  if (authHint.test(forHint || "")) {
    box.appendChild(
      h("span", {
        class: "hint",
        text: "If this is an authentication problem, store a fresh token with `mosso auth claude` (or `mosso auth github` for GitHub errors); Mosso reads it again on the next request, without a restart.",
      }),
    );
  }
  return box;
}

function markPendingTools() {
  for (const t of state.tools.values()) {
    if (t.state.classList.contains("pending")) {
      t.state.className = "tool-state";
      t.state.textContent = "–";
    }
  }
}

// container returns where an event belongs: inside its subagent's tool, if any.
function container(ev) {
  if (ev.parent_tool_use_id) {
    const parent = state.tools.get(ev.parent_tool_use_id);
    if (parent) return parent.children;
  }
  return $("entries");
}

function renderEvent(ev) {
  const where = container(ev);
  if (ev.type === "text") {
    const div = h("div", { class: "entry md" });
    div.appendChild(window.renderMarkdown(ev.text));
    where.appendChild(div);
  } else if (ev.type === "tool_use") {
    const input = ev.input || {};
    const stateEl = h("span", { class: "tool-state pending", text: "running" });
    const body = h("div", { class: "tool-body" });
    toolInput(ev.name, input, body);
    const children = h("div", { class: "subagent" });
    const details = h(
      "details",
      { class: "entry tool" },
      h("summary", {}, h("span", { class: "tool-name", text: ev.name }), h("span", { class: "tool-desc", text: toolSummary(ev.name, input) }), stateEl),
      body,
    );
    if (ev.name === "Task" || ev.name === "Agent") details.appendChild(children);
    where.appendChild(details);
    state.tools.set(ev.tool_use_id, { details, state: stateEl, body, children });
  } else if (ev.type === "tool_result") {
    const tool = state.tools.get(ev.tool_use_id);
    const pre = h("pre", { class: ev.is_error ? "failed" : "", text: ev.content || "(no output)" });
    if (!tool) {
      where.appendChild(h("details", { class: "entry tool" }, h("summary", {}, h("span", { class: "tool-name", text: "Tool result" })), h("div", { class: "tool-body" }, pre)));
      return;
    }
    tool.state.className = "tool-state " + (ev.is_error ? "failed" : "ok");
    tool.state.textContent = ev.is_error ? "failed" : "✓";
    tool.body.appendChild(h("span", { class: "label", text: ev.is_error ? "Error" : "Result" }));
    tool.body.appendChild(pre);
  }
}

function relPath(p) {
  const dir = state.ws && state.ws.dir;
  if (dir && typeof p === "string" && p.startsWith(dir + "/")) return p.slice(dir.length + 1);
  return p;
}

function toolSummary(name, input) {
  const pick = (...keys) => {
    for (const k of keys) if (typeof input[k] === "string" && input[k]) return input[k];
    return "";
  };
  switch (name) {
    case "Bash":
      return firstLine(pick("command"), 200);
    case "Read":
    case "Write":
    case "Edit":
    case "MultiEdit":
    case "NotebookEdit":
      return relPath(pick("file_path", "notebook_path"));
    case "Glob":
      return pick("pattern");
    case "Grep":
      return pick("pattern") + (input.path ? "  in " + relPath(input.path) : "");
    case "WebFetch":
      return pick("url");
    case "WebSearch":
      return pick("query");
    case "Task":
    case "Agent":
      return pick("description", "prompt");
    case "TodoWrite":
      return Array.isArray(input.todos) ? `${input.todos.length} items` : "";
    default: {
      const s = Object.values(input).find((v) => typeof v === "string");
      return s ? firstLine(s, 160) : "";
    }
  }
}

const maxShown = 20000;
function clip(s) {
  s = String(s ?? "");
  return s.length > maxShown ? s.slice(0, maxShown) + "\n… (truncated in the UI)" : s;
}

function diff(oldText, newText) {
  const pre = h("pre");
  for (const l of clip(oldText).split("\n")) pre.appendChild(h("span", { class: "diff-del", text: "- " + l + "\n" }));
  for (const l of clip(newText).split("\n")) pre.appendChild(h("span", { class: "diff-add", text: "+ " + l + "\n" }));
  return pre;
}

function toolInput(name, input, body) {
  const label = (t) => body.appendChild(h("span", { class: "label", text: t }));
  switch (name) {
    case "Bash":
      if (input.description) body.appendChild(h("div", { class: "muted", text: input.description }));
      label("Command");
      body.appendChild(h("pre", { text: clip(input.command) }));
      return;
    case "Edit":
      label(relPath(input.file_path) || "Edit");
      body.appendChild(diff(input.old_string, input.new_string));
      return;
    case "MultiEdit":
      label(relPath(input.file_path) || "Edit");
      for (const ed of input.edits || []) body.appendChild(diff(ed.old_string, ed.new_string));
      return;
    case "Write":
      label(relPath(input.file_path) || "Write");
      body.appendChild(h("pre", { text: clip(input.content) }));
      return;
    case "TodoWrite":
      body.appendChild(
        h(
          "ul",
          { class: "todo-list" },
          ...(input.todos || []).map((t) =>
            h("li", { class: t.status, text: (t.status === "completed" ? "☑ " : t.status === "in_progress" ? "▸ " : "☐ ") + (t.content || t.activeForm || "") }),
          ),
        ),
      );
      return;
    case "Task":
    case "Agent":
      label("Prompt");
      body.appendChild(h("pre", { text: clip(input.prompt) }));
      return;
    default:
      label("Input");
      body.appendChild(h("pre", { text: clip(JSON.stringify(input, null, 2)) }));
  }
}

// ---------- actions ----------

async function sendPrompt() {
  const prompt = $("prompt");
  const text = prompt.value;
  if (!text.trim() || !state.ws || isWorking() || state.sending) return;
  const name = state.selected;
  state.sending = true;
  render();
  try {
    await api("POST", wsPath(name) + "/chat", { prompt: text });
    if (state.selected === name) prompt.value = "";
    state.drafts.delete(name);
    hideBanner();
  } catch (e) {
    showBanner(e.status === 409 ? "Claude is still working on the previous message." : "Could not send the message: " + e.message);
  } finally {
    state.sending = false;
    refreshList();
  }
}

async function stopTurn() {
  if (!state.selected) return;
  $("stop").disabled = true;
  try {
    await api("POST", wsPath(state.selected) + "/chat/stop", {});
  } catch (e) {
    if (e.status !== 409) showBanner("Could not stop Claude: " + e.message);
  } finally {
    $("stop").disabled = false;
  }
}

async function terminate() {
  const ws = state.ws;
  if (!ws) return;
  const msg =
    `Terminate the workspace for ${ws.repo}?\n\n` +
    "This destroys the Vercel Sandbox with its files and the Claude conversation. Unpublished changes are lost. " +
    (ws.pull_request ? `The branch and PR #${ws.pull_request.number} stay on GitHub.` : "Nothing has been published yet.");
  if (!confirm(msg)) return;
  const btn = $("terminate");
  btn.disabled = true;
  btn.textContent = "Terminating…";
  try {
    await api("DELETE", wsPath(ws.name));
    state.drafts.delete(ws.name);
    if (state.selected === ws.name) select(null);
  } catch (e) {
    showBanner("Could not terminate the workspace: " + e.message + (e.status === 502 ? "\nCheck the Vercel dashboard for a leftover sandbox." : ""));
  } finally {
    btn.disabled = false;
    btn.textContent = "Terminate";
    refreshList();
  }
}

function openPublish() {
  const ws = state.ws;
  if (!ws) return;
  const update = !!ws.pull_request;
  const prompts = state.entries.filter((e) => e.type === "user");
  $("publish-heading").textContent = update ? `Update pull request #${ws.pull_request.number}` : "Create pull request";
  $("publish-title-label").textContent = update ? "Commit message" : "Title";
  const source = update ? prompts[prompts.length - 1] : prompts[0];
  $("publish-title").value = source ? firstLine(source.text, 72) : "";
  $("publish-body").value = "";
  $("publish-draft").checked = false;
  $("publish-body-field").classList.toggle("hidden", update);
  $("publish-draft-field").classList.toggle("hidden", update);
  $("publish-error").classList.add("hidden");
  $("publish-submit").disabled = false;
  $("publish-submit").textContent = update ? "Push update" : "Create pull request";
  $("publish-dialog").showModal();
  $("publish-title").select();
}

async function submitPublish(ev) {
  ev.preventDefault();
  const ws = state.ws;
  if (!ws) return;
  const title = $("publish-title").value.trim();
  if (!title) return;
  const body = { title };
  if (!ws.pull_request) {
    body.body = $("publish-body").value;
    body.draft = $("publish-draft").checked;
  }
  const err = $("publish-error");
  err.classList.add("hidden");
  $("publish-submit").disabled = true;
  $("publish-submit").textContent = "Publishing…";
  state.publishing = true;
  render();
  try {
    await api("POST", wsPath(ws.name) + "/pull-request", body);
    $("publish-dialog").close();
  } catch (e) {
    if (e.status === 422) {
      $("publish-dialog").close(); // the chat shows a "nothing to publish" notice
    } else {
      err.textContent = e.status === 409 ? "Claude is still working; publish when it has finished." : e.message;
      err.classList.remove("hidden");
      $("publish-submit").disabled = false;
      $("publish-submit").textContent = "Try again";
    }
  } finally {
    state.publishing = false;
    refreshList();
  }
}

async function openCreate() {
  const repo = $("create-repo");
  repo.value = localStorage.getItem("mosso.lastRepo") || "";
  $("create-base").value = "";
  $("create-error").classList.add("hidden");
  setCreating(false);
  $("create-dialog").showModal();
  repo.select();
  if (repo.value) loadBranches(repo.value);
  if (!state.repos) {
    try {
      state.repos = (await api("GET", "/github/repos")).repos;
      $("repo-options").replaceChildren(...state.repos.map((r) => h("option", { value: r.full_name, label: r.private ? "private" : "" })));
    } catch (e) {
      /* typing a repository still works */
    }
  }
}

let branchesFor = "";
async function loadBranches(repo) {
  repo = repo.trim();
  if (!/^[\w.-]+\/[\w.-]+$/.test(repo) || repo === branchesFor) return;
  branchesFor = repo;
  const list = $("branch-options");
  list.replaceChildren();
  const known = (state.repos || []).find((r) => r.full_name.toLowerCase() === repo.toLowerCase());
  $("create-base").placeholder = known ? `${known.default_branch} (default)` : "default branch";
  try {
    const { branches } = await api("GET", `/github/repos/${repo}/branches`);
    if (branchesFor === repo) list.replaceChildren(...branches.map((b) => h("option", { value: b })));
  } catch (e) {
    /* the create request reports a bad repository */
  }
}

function setCreating(on) {
  $("create-submit").disabled = on;
  $("create-submit").textContent = on ? "Creating… (about a minute)" : "Create";
  $("create-repo").disabled = on;
  $("create-base").disabled = on;
}

async function submitCreate(ev) {
  ev.preventDefault();
  const repo = $("create-repo").value.trim().replace(/^https:\/\/github\.com\//, "").replace(/\.git$/, "").replace(/\/$/, "");
  const base = $("create-base").value.trim();
  if (!repo) return;
  const err = $("create-error");
  err.classList.add("hidden");
  setCreating(true);
  // Show the workspace in the sidebar as soon as the server lists it.
  const poll = setInterval(refreshList, 2000);
  try {
    const ws = await api("POST", "/workspaces", base ? { repo, base } : { repo });
    localStorage.setItem("mosso.lastRepo", repo);
    $("create-dialog").close();
    await refreshList();
    select(ws.name);
  } catch (e) {
    err.textContent = e.message;
    err.classList.remove("hidden");
  } finally {
    clearInterval(poll);
    setCreating(false);
  }
}

// ---------- wiring ----------

function init() {
  $("new-workspace").addEventListener("click", openCreate);
  $("empty-new").addEventListener("click", openCreate);
  $("create-form").addEventListener("submit", submitCreate);
  $("create-cancel").addEventListener("click", () => $("create-dialog").close());
  $("create-repo").addEventListener("change", (e) => loadBranches(e.target.value));
  $("create-repo").addEventListener("input", (e) => {
    if ((state.repos || []).some((r) => r.full_name === e.target.value)) loadBranches(e.target.value);
  });
  // Closing the dialog while a create is running does not cancel it: the
  // workspace appears in the sidebar when it is ready.
  $("publish").addEventListener("click", openPublish);
  $("publish-form").addEventListener("submit", submitPublish);
  $("publish-cancel").addEventListener("click", () => $("publish-dialog").close());
  $("terminate").addEventListener("click", terminate);
  $("stop").addEventListener("click", stopTurn);
  $("banner-close").addEventListener("click", hideBanner);
  $("composer").addEventListener("submit", (e) => {
    e.preventDefault();
    sendPrompt();
  });
  $("prompt").addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      sendPrompt();
    }
  });
  $("prompt").addEventListener("input", (e) => {
    const t = e.target;
    t.style.height = "auto";
    t.style.height = t.scrollHeight + 2 + "px";
  });

  refreshList().then(loadChecks);
  setInterval(refreshList, 5000);
  setInterval(() => {
    renderExpiry();
    if (isWorking()) renderWorking();
  }, 1000);
}

init();
