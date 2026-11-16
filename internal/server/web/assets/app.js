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
  pending: new Map(), // workspace name -> attachments being composed
  sending: false,
  publishing: false,
  resuming: false,
  archiving: false,
  showArchived: false, // the sidebar's Archived section is open
  repos: null,
  github: null, // GET /github/auth: whether GitHub is connected (never the token)
  vercel: null, // GET /vercel/auth: whether sandboxes can be used at all
  settings: null, // GET /settings: the defaults for new workspaces and the models to choose from
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

// noVercel reports that Mosso has no Vercel credentials: saved workspaces
// and their conversations can be read (and archived), but nothing that
// needs a sandbox works.
// modelName returns a model's display name, or its ID if it is no longer
// offered. Workspaces from before models could be chosen have none.
function modelName(id) {
  if (!id) return "Claude Code default";
  const m = ((state.settings && state.settings.models) || []).find((m) => m.id === id);
  return m ? m.name : id;
}

// modelOptions fills a <select> with the models, selecting one.
function modelOptions(select, selected) {
  const models = (state.settings && state.settings.models) || [];
  select.replaceChildren(...models.map((m) => h("option", { value: m.id, text: m.name })));
  select.value = selected;
}

// imageName describes a sandbox image; "" is Mosso's built-in runtime.
function imageName(image) {
  return image || ((state.settings && state.settings.built_in_runtime) || "node22") + " (built-in)";
}

function noVercel() {
  return !!state.vercel && !state.vercel.configured;
}

const vercelHelp = "Vercel Sandbox is not configured: set VERCEL_TOKEN, VERCEL_TEAM_ID and VERCEL_PROJECT_ID and restart Mosso.";

const authHint = /\b(401|403)\b|auth|log ?in|oauth|token|credential|unauthori[sz]ed/i;

// ---------- configuration checks ----------

async function loadChecks() {
  const list = $("checks");
  const item = (label, ok, detail, action) =>
    h(
      "li",
      {},
      h("span", { class: ok ? "check-ok" : "check-bad", text: ok ? "●" : "○" }),
      h("span", { text: label }),
      h("span", { class: "check-detail", title: detail || "", text: detail || "" }),
      action || null,
    );
  const [claude, github, vercel, settings] = await Promise.all([
    api("GET", "/claude/auth").catch((e) => ({ error: e.message })),
    api("GET", "/github/auth").catch((e) => ({ error: e.message })),
    api("GET", "/vercel/auth").catch(() => null),
    api("GET", "/settings").catch(() => null),
  ]);
  state.github = github;
  state.vercel = vercel;
  state.settings = settings;
  const ghOK = github.configured && !github.error;
  let ghDetail = !github.configured ? "not connected" : github.login ? "@" + github.login : "token problem";
  const days = github.expires_at ? Math.floor((new Date(github.expires_at).getTime() - Date.now()) / 86400000) : null;
  if (ghOK && days !== null && days < 14) ghDetail += days < 0 ? " · expired" : ` · expires in ${days}d`;
  const ghButton = h("button", { type: "button", class: "small check-action", text: github.configured ? "Replace" : "Connect", onclick: openGitHub });
  list.replaceChildren(
    item("Vercel", !noVercel(), noVercel() ? "not configured · read-only" : ""),
    item("Claude", claude.configured, claude.configured ? claude.source : "run mosso auth claude"),
    item("GitHub", ghOK, ghDetail, ghButton),
  );
  const ghItem = list.lastChild;
  if (settings) {
    const modelButton = h("button", { type: "button", class: "small check-action", text: "Change", onclick: openSettings });
    list.append(item("Model", true, modelName(settings.default_model), modelButton));
    list.lastChild.title = "Default model for new workspaces";
    const envButton = h("button", { type: "button", class: "small check-action", text: "Change", onclick: openSettings });
    list.append(item("Sandbox", true, imageName(settings.default_image) + (settings.default_setup_script ? " · setup script" : ""), envButton));
    list.lastChild.title = "Default image and setup script for new workspaces";
  }
  ghItem.title = github.error || (github.expires_at ? "Token expires " + new Date(github.expires_at).toLocaleDateString() : "");
  if (ghOK && days !== null && days < 14) ghItem.querySelector(".check-detail").classList.add("soon");
  render();
}

// ---------- connecting GitHub ----------

function openGitHub() {
  const gh = state.github || {};
  $("github-heading").textContent = gh.configured ? "Replace the GitHub token" : "Connect GitHub";
  const current = $("github-current");
  if (gh.configured) {
    const where = { keychain: "the macOS Keychain", env: "MOSSO_GITHUB_TOKEN", memory: "memory, until Mosso stops" }[gh.source] || gh.source;
    let text = gh.login ? `Connected as @${gh.login}, with a token from ${where}.` : `A token is configured (${where}), but GitHub reports a problem: ${gh.error || "unknown"}.`;
    if (gh.expires_at) text += ` It expires on ${new Date(gh.expires_at).toLocaleDateString()}.`;
    current.textContent = text + " A new token replaces it for every workspace, without a restart.";
    current.classList.remove("hidden");
  } else {
    current.classList.add("hidden");
  }
  $("github-create").href = gh.create_url || "https://github.com/settings/personal-access-tokens/new";
  $("github-storage").textContent =
    gh.storage === "memory"
      ? "This system has no macOS Keychain, so Mosso keeps the token in memory until it stops (set MOSSO_GITHUB_TOKEN to keep it across restarts). The browser does not store it."
      : "Mosso checks the token with GitHub and saves it in your macOS Keychain; it is used for every workspace and never sent into a sandbox. The browser does not store it.";
  $("github-token").value = "";
  $("github-error").classList.add("hidden");
  $("github-submit").disabled = false;
  $("github-submit").textContent = "Connect";
  $("github-dialog").showModal();
  $("github-token").focus();
}

function closeGitHub() {
  $("github-token").value = "";
  $("github-dialog").close();
}

async function submitGitHub(ev) {
  ev.preventDefault();
  const input = $("github-token");
  const token = input.value.trim();
  if (!token) return;
  // Clear the field at once: the token lives only in this request.
  input.value = "";
  const err = $("github-error");
  err.classList.add("hidden");
  $("github-submit").disabled = true;
  $("github-submit").textContent = "Checking…";
  try {
    await api("POST", "/github/auth", { token });
    closeGitHub();
    state.repos = null; // the new token may see other repositories
    hideBanner();
    await loadChecks();
  } catch (e) {
    err.textContent = e.message;
    err.classList.remove("hidden");
    $("github-submit").disabled = false;
    $("github-submit").textContent = "Try again";
    input.focus();
  }
}

// ---------- settings for new workspaces ----------

function openSettings() {
  const s = state.settings;
  if (!s) return;
  modelOptions($("settings-model"), s.default_model);
  $("settings-image").value = s.default_image || "";
  $("settings-image-help").textContent = `Empty uses Mosso's built-in ${imageName("")} runtime. Otherwise a Vercel Container Registry reference: a repository in Mosso's Vercel project ("my-image:v1"), one shared with your team ("team/project/my-image"), or a Vercel managed image ("vercel/sandbox/universal"). It needs bash, curl and git.`;
  $("settings-setup").value = s.default_setup_script || "";
  $("settings-error").classList.add("hidden");
  $("settings-submit").disabled = false;
  $("settings-dialog").showModal();
  $("settings-model").focus();
}

async function submitSettings(ev) {
  ev.preventDefault();
  const err = $("settings-error");
  err.classList.add("hidden");
  $("settings-submit").disabled = true;
  try {
    state.settings = await api("POST", "/settings", {
      default_model: $("settings-model").value,
      default_image: $("settings-image").value.trim(),
      default_setup_script: $("settings-setup").value,
    });
    $("settings-dialog").close();
    await loadChecks();
  } catch (e) {
    err.textContent = e.message;
    err.classList.remove("hidden");
    $("settings-submit").disabled = false;
  }
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
    // Open the workspace in the URL, else the newest one with a sandbox,
    // else the newest in the history. Archived ones only open when asked.
    const fromHash = decodeURIComponent(location.hash.replace(/^#\/?/, ""));
    const groups = groupWorkspaces(state.workspaces);
    const pick = state.workspaces.find((w) => w.name === fromHash) || groups.active[groups.active.length - 1] || groups.history[0];
    if (pick) select(pick.name);
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
  const item = (w) => {
    const past = isTerminated(w);
    const branch = w.branch.replace(/^mosso\//, "");
    return h(
      "button",
      { type: "button", class: "ws-item" + (past ? " past" : "") + (w.name === state.selected ? " selected" : ""), title: `${w.repo} (${w.name})`, onclick: () => select(w.name) },
      h("div", { class: "ws-item-top" }, h("span", { class: "dot " + w.status, title: statusLabel(w) }), h("span", { class: "ws-item-repo", text: shortRepo(w.repo) })),
      h("div", { class: "ws-item-sub", text: past ? `${shortDate(w.created_at)} · ${branch}` : `${w.base} → ${branch}` }),
    );
  };
  const groups = groupWorkspaces(state.workspaces);
  const nodes = groups.active.map(item);
  if (!groups.active.length) nodes.push(h("div", { class: "list-empty", text: "No running workspaces." }));
  if (groups.history.length) nodes.push(h("div", { class: "list-heading", text: "History" }), ...groups.history.map(item));
  if (groups.archived.length) {
    // Archived workspaces stay out of the way until asked for.
    const open = state.showArchived || groups.archived.some((w) => w.name === state.selected);
    const details = h("details", { class: "archived", open }, h("summary", { class: "list-heading", text: `Archived (${groups.archived.length})` }), ...groups.archived.map(item));
    details.addEventListener("toggle", () => (state.showArchived = details.open));
    nodes.push(details);
  }
  nav.replaceChildren(...nodes);
}

// isTerminated reports whether the workspace is history: it was terminated,
// so its sandbox is deleted and only the conversation is left.
function isTerminated(ws) {
  return !!ws && ws.sandbox_state === "terminated";
}

// groupWorkspaces splits the list for the sidebar: workspaces that still
// have a sandbox (oldest first, as created), the history of terminated
// ones, and the archived ones (both newest first).
function groupWorkspaces(list) {
  const groups = { active: [], history: [], archived: [] };
  for (const w of list) (w.archived_at ? groups.archived : isTerminated(w) ? groups.history : groups.active).push(w);
  groups.history.reverse();
  groups.archived.reverse();
  return groups;
}

function shortDate(iso) {
  const d = new Date(iso);
  const opts = { month: "short", day: "numeric" };
  if (d.getFullYear() !== new Date().getFullYear()) opts.year = "numeric";
  return d.toLocaleDateString(undefined, opts);
}

function statusLabel(ws) {
  switch (ws.status) {
    case "creating":
      return "Setting up";
    case "connecting":
      return "Connecting";
    case "working":
      return "Working";
    case "stopped":
      return "Stopped";
    case "terminated":
      return ws.archived_at ? "Archived" : "Terminated";
    case "error":
      return ws.sandbox_state === "gone" ? "Gone" : "Error";
    default:
      return "Ready";
  }
}

// sandboxGone reports whether the workspace has no sandbox any more: it was
// deleted outside Mosso or expired ("gone"), or the workspace was terminated.
function sandboxGone(ws) {
  return !!ws && (ws.sandbox_state === "gone" || isTerminated(ws));
}

function renderHeader() {
  const ws = state.ws;
  if (!ws) return;
  const repo = $("ws-repo");
  repo.textContent = ws.repo;
  repo.href = "https://github.com/" + ws.repo;
  $("ws-branch").textContent = `${ws.base} → ${ws.branch}`;
  $("ws-branch").title = "Base branch → working branch";
  const model = $("ws-model");
  model.textContent = modelName(ws.model);
  model.title = ws.model ? `Claude Code runs ${ws.model} in this workspace` : "This workspace predates model selection: Claude Code uses its default model";
  model.classList.remove("hidden");
  // The sandbox environment, if the workspace has a custom one.
  const env = $("ws-env");
  env.classList.toggle("hidden", !ws.image && !ws.setup_script);
  env.textContent = imageName(ws.image) + (ws.setup_script ? " · setup" : "");
  env.title =
    (ws.image ? `The sandbox was created from the image ${ws.image}.` : "The sandbox runs Mosso's built-in runtime.") +
    (ws.setup_script ? "\n\nSetup script, run once after the clone:\n\n" + ws.setup_script : "");
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
  const busy = ws.status === "working" || ws.status === "creating" || ws.status === "connecting";
  // History is read-only: it can only be archived (or brought back).
  const past = isTerminated(ws);
  const archive = $("archive");
  archive.classList.toggle("hidden", !past);
  archive.disabled = state.archiving;
  archive.textContent = ws.archived_at ? "Unarchive" : "Archive";
  archive.title = ws.archived_at ? "Move this workspace back to the history" : "Move this workspace out of the history. It stays readable under Archived.";
  // A terminated workspace only keeps the button to save the termination
  // again, if the database write failed the first time.
  $("terminate").classList.toggle("hidden", past && !ws.unsaved);
  const publish = $("publish");
  publish.classList.toggle("hidden", past);
  publish.textContent = state.publishing ? "Publishing…" : ws.pull_request ? "Update PR" : "Create PR";
  publish.disabled = busy || state.publishing || sandboxGone(ws) || noVercel();
  publish.title = noVercel() ? vercelHelp : busy ? "Wait for the current task to finish" : "Commit and push the sandbox's changes as a pull request (a stopped sandbox is resumed first)";
  const terminateBtn = $("terminate");
  if (noVercel()) terminateBtn.disabled = true; // deleting a sandbox needs Vercel
  terminateBtn.title = noVercel() ? vercelHelp : past ? "The sandbox is deleted, but that could not be saved. Try saving it again." : "";
  if (!terminateBtn.disabled) terminateBtn.textContent = past ? "Save again" : "Terminate";
  const resume = $("resume");
  resume.classList.toggle("hidden", ws.status !== "stopped");
  resume.disabled = state.resuming || noVercel();
  resume.textContent = state.resuming ? "Resuming…" : "Resume sandbox";

  const err = $("ws-error");
  if (ws.error && ws.status === "error") {
    err.textContent = ws.error;
    err.classList.remove("hidden");
  } else if (ws.unsaved) {
    err.textContent =
      "Mosso could not save this workspace's latest changes to its database (the server log says why). They are kept in memory and saved with the next change; if Mosso restarts first, they are lost." +
      (past ? " The sandbox is deleted either way." : "");
    err.classList.remove("hidden");
  } else {
    err.classList.add("hidden");
  }
  renderExpiry();
}

function renderExpiry() {
  const ws = state.ws;
  const el = $("ws-expiry");
  if (!ws) return;
  if (isTerminated(ws)) {
    el.textContent = ws.archived_at ? "Archived " + shortDate(ws.archived_at) : "Created " + shortDate(ws.created_at);
    el.className = "muted";
    el.title = "This workspace was terminated: its sandbox and snapshots are deleted. The conversation is kept as history.";
    return;
  }
  if (noVercel()) {
    el.textContent = "Not connected · as last saved";
    el.className = "muted";
    el.title = vercelHelp;
    return;
  }
  if (sandboxGone(ws)) {
    el.textContent = "Sandbox gone";
    el.className = "soon";
    el.title = "The sandbox was deleted or its snapshot expired. The conversation is kept; terminate the workspace to move it to the history.";
    return;
  }
  if (ws.status === "connecting") {
    el.textContent = "Checking the sandbox…";
    el.className = "muted";
    el.title = "Mosso restarted and is reconnecting to the sandbox.";
    return;
  }
  if (!ws.expires_at || ws.status === "stopped") {
    el.textContent = "Sandbox stopped";
    el.className = "muted";
    el.title = "The sandbox session ended (sessions last up to 45 minutes). Its files and the conversation are saved; sending a message or clicking Resume starts a new session.";
    return;
  }
  const left = new Date(ws.expires_at).getTime() - Date.now();
  if (left <= 0) {
    el.textContent = "Session ended";
    el.className = "muted";
  } else {
    el.textContent = "Session ends in " + (left < 60000 ? "under a minute" : Math.floor(left / 60000) + " min");
    el.className = left < 5 * 60000 ? "soon" : "muted";
  }
  el.title = "Vercel stops the sandbox session at " + new Date(ws.expires_at).toLocaleTimeString() + ". Claude's work is saved in the sandbox and resumes with your next message.";
}

function isWorking() {
  return state.ws && (state.ws.status === "working" || state.ws.status === "creating" || state.ws.status === "connecting");
}

function lastEntryOf(types) {
  for (let i = state.entries.length - 1; i >= 0; i--) if (types.includes(state.entries[i].type)) return state.entries[i];
  return null;
}

function claudeRunning() {
  // A turn is running if the latest prompt has not been followed by its end.
  // Notices during a turn (installing, fetching from GitHub) do not end it.
  for (let i = state.entries.length - 1; i >= 0; i--) {
    const e = state.entries[i];
    if (e.type === "user") return true;
    if (["result", "error", "pull_request"].includes(e.type)) return false;
    if (e.type === "notice" && /^(Stopped|Interrupted|Publishing|Nothing to publish)/.test(e.text)) return false;
  }
  return false;
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
    if (ws.status === "creating") {
      text =
        {
          cloning: "Setting up the workspace: cloning the repository…",
          setup: "Running the setup script…",
          installing: "Setting up the workspace: installing Claude Code and starting the worker…",
        }[ws.step] || "Setting up the workspace: provisioning the sandbox, cloning and installing Claude Code…";
      if (ws.step === "setup") since = lastEntryOf(["notice"]);
      // A first message given with the workspace waits for the setup.
      if (lastEntryOf(["user"])) text += " Claude starts on your message when it is ready.";
    }
    else if (ws.status === "connecting") text = "Reconnecting to the sandbox after a restart…";
    else if (running) {
      text = "Claude is working…";
      since = lastEntryOf(["user"]);
    } else if (state.publishing || (lastEntryOf(["notice"]) || {}).text?.startsWith("Publishing")) {
      text = "Publishing to GitHub…";
      since = lastEntryOf(["notice"]);
    }
    $("working-text").textContent = text;
    $("working-time").textContent = since ? fmtDuration(Date.now() - new Date(since.time).getTime()) : "";
    // Nothing runs until the workspace is set up, so there is nothing to stop.
    $("stop").classList.toggle("hidden", !running || ws.status === "creating");
  }

  const prompt = $("prompt");
  const send = $("send");
  const hint = $("composer-hint");
  const pending = (state.selected && state.pending.get(state.selected)) || [];
  const uploading = pending.some((i) => i.status !== "done");
  const gone = sandboxGone(ws);
  // Without Vercel a workspace is shown as last saved and cannot be used.
  const offline = noVercel() && !gone;
  const blocked = busy || state.sending || !ws || uploading || gone || offline;
  send.disabled = blocked;
  prompt.disabled = gone || offline;
  // Nothing can be sent to a terminated workspace, so there is no composer.
  $("composer").classList.toggle("hidden", isTerminated(ws));
  if (offline) {
    prompt.placeholder = "Read-only: this workspace is shown as Mosso last saved it. " + vercelHelp;
    hint.textContent = "Vercel is not configured";
  } else if (gone) {
    prompt.placeholder = "The sandbox is gone. The conversation is kept; terminate the workspace to move it to the history.";
    hint.textContent = "";
  } else if (ws && ws.status === "creating") {
    prompt.placeholder = "The workspace is being set up…";
    hint.textContent = "";
  } else if (ws && ws.status === "stopped") {
    prompt.placeholder = "Ask Claude to make a change…  (the stopped sandbox resumes first, which takes a few seconds)";
    hint.textContent = "Sandbox stopped; sending resumes it.";
  } else if (busy) {
    prompt.placeholder = "Claude is busy. You can type your next message; send it when the current task finishes.";
    hint.textContent = "Busy";
  } else {
    prompt.placeholder = "Ask Claude to make a change…  (Enter to send, Shift+Enter for a new line)";
    hint.textContent = uploading ? "Waiting for attachments…" : state.entries.some((e) => e.type === "user") ? "Follow-ups continue the same conversation." : "";
  }
}

function render() {
  renderSidebar();
  const hasWs = !!(state.selected && state.ws);
  $("workspace").classList.toggle("hidden", !hasWs);
  $("empty").classList.toggle("hidden", hasWs);
  const emptyErr = $("empty-error");
  if (state.listError || noVercel()) {
    emptyErr.textContent = state.listError ? state.listError.message : vercelHelp + " Saved workspaces can still be read.";
    emptyErr.classList.remove("hidden");
  } else {
    emptyErr.classList.add("hidden");
  }
  $("empty-new").disabled = !!state.listError || noVercel();
  $("empty-connect").classList.toggle("hidden", !state.github || !!state.github.configured);
  $("new-workspace").disabled = !!state.listError || noVercel();
  $("new-workspace").title = noVercel() ? vercelHelp : "Create a workspace";
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
  renderAttachments();
  history.replaceState(null, "", name ? "#" + encodeURIComponent(name) : location.pathname);
  hideBanner();
  if (name) {
    follow(name);
    if (!isTerminated(state.ws)) setTimeout(() => $("prompt").focus(), 0);
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
      box.appendChild(
        h(
          "div",
          { class: "entry user-msg" },
          h("span", { class: "who", text: "You" }),
          e.text,
          e.attachments && e.attachments.length
            ? h("ul", { class: "attachments" }, ...e.attachments.map((a) => attachmentChip(a, { url: attachmentURL(state.selected, a.id) })))
            : null,
        ),
      );
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
    case "setup": {
      // Consecutive pieces of setup output share one log.
      let log = box.lastElementChild;
      if (!log || !log.classList.contains("setup-log")) {
        log = h("details", { class: "entry setup-log", open: state.ws && state.ws.status === "creating" }, h("summary", { text: "Setup script output" }), h("pre"));
        box.appendChild(log);
      }
      const pre = log.querySelector("pre");
      const atEnd = pre.scrollHeight - pre.scrollTop - pre.clientHeight < 20;
      pre.textContent += e.text;
      if (atEnd) pre.scrollTop = pre.scrollHeight;
      break;
    }
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
          h("strong", { text: pr.created ? "Opened pull request" : pr.forced ? "Updated pull request (rebased, pushed with --force-with-lease)" : "Updated pull request" }),
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
        text: "If this is an authentication problem, store a fresh token with `mosso auth claude`, or for GitHub errors use Connect GitHub (Replace) in the sidebar; Mosso uses the new token from the next request on, without a restart.",
      }),
    );
  }
  if (/github|token|push/i.test(forHint || "") && /expired|revoked|lacks access|not found/i.test(forHint || "")) {
    box.appendChild(h("button", { type: "button", class: "small", text: "Replace the GitHub token", onclick: openGitHub }));
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

// ---------- attachments ----------

const attachmentURL = (ws, id) => wsPath(ws) + "/attachments/" + encodeURIComponent(id);
let nextAttachmentKey = 0;

const pendingFor = (name) => {
  if (!state.pending.has(name)) state.pending.set(name, []);
  return state.pending.get(name);
};

// attachmentChip renders an attachment, in the composer (opts.item) or in
// the history (opts.url): a thumbnail for images, an icon for files.
function attachmentChip(a, opts) {
  const item = opts.item;
  const li = h("li", { class: "attachment" + (item && item.status === "error" ? " failed" : "") });
  const label = a.name + " (" + Attach.formatSize(a.size) + ")";
  const preview = item ? item.preview : opts.url;
  let visual;
  if (a.image && preview) {
    const img = h("img", { alt: a.name });
    img.src = preview;
    img.addEventListener("error", () => {
      li.classList.add("gone");
      visual.replaceChildren("—");
      visual.disabled = true;
      sub.textContent = "Not available while the sandbox is stopped or gone; resume it and reload";
    });
    visual = h("button", { class: "thumb", type: "button", title: "Preview " + a.name, "aria-label": "Preview " + label, onclick: () => openPreview(preview, label) }, img);
  } else {
    visual = h("span", { class: "file-icon", "aria-hidden": "true", text: Attach.extension(a.name) });
  }
  const sub = h("span", { class: "sub", text: Attach.formatSize(a.size) });
  const meta = h("span", { class: "meta" }, h("span", { class: "fname", text: a.name, title: a.name }), sub);
  if (item && item.status === "uploading") {
    const bar = h("progress", { max: 1, value: item.progress, "aria-label": "Uploading " + a.name });
    meta.appendChild(bar);
    sub.textContent = "Uploading… " + Math.round(item.progress * 100) + "%";
  } else if (item && item.status === "error") {
    sub.className = "sub err";
    sub.textContent = item.error;
  }
  li.append(visual, meta);
  if (item) {
    if (item.status === "error" && item.file) li.appendChild(h("button", { class: "retry", type: "button", title: "Retry", "aria-label": "Retry " + a.name, text: "↻", onclick: () => startUpload(item) }));
    li.appendChild(h("button", { class: "x", type: "button", title: "Remove", "aria-label": "Remove " + a.name, text: "✕", onclick: () => removeAttachment(item) }));
  } else if (!a.image || !preview) {
    li.appendChild(h("button", { class: "retry", type: "button", title: "Download", "aria-label": "Download " + label, text: "⤓", onclick: () => downloadAttachment(a, opts.url, sub, li) }));
  }
  return li;
}

function openPreview(src, label) {
  $("preview-img").src = src;
  $("preview-img").alt = label;
  $("preview-name").textContent = label;
  $("preview-dialog").showModal();
}

// downloadAttachment saves a file from the sandbox through a blob, so a
// workspace that is gone is reported instead of saving an error page.
async function downloadAttachment(a, url, sub, li) {
  try {
    const res = await fetch(url + "?download=1");
    if (!res.ok) throw new Error(res.status === 410 ? "gone" : "failed");
    const blobURL = URL.createObjectURL(await res.blob());
    const link = h("a", { download: a.name, href: blobURL });
    document.body.appendChild(link);
    link.click();
    link.remove();
    setTimeout(() => URL.revokeObjectURL(blobURL), 10000);
  } catch (e) {
    li.classList.add("gone");
    sub.textContent = e.message === "gone" ? "Not available while the sandbox is stopped or gone; resume it and try again" : "Download failed";
  }
}

function renderAttachments() {
  const list = $("attachments");
  const items = (state.selected && state.pending.get(state.selected)) || [];
  list.classList.toggle("hidden", items.length === 0);
  list.replaceChildren(...items.map((item) => attachmentChip({ name: item.name, size: item.size, image: item.image }, { item })));
  render();
}

// addFiles is where paste, drop and the file picker all end up.
function addFiles(files) {
  if (!state.ws || state.ws.status === "creating") {
    showBanner("The workspace is still being set up; attach files in a moment.");
    return;
  }
  if (sandboxGone(state.ws) || noVercel()) return; // read-only: no sandbox to upload to
  const name = state.selected;
  const items = pendingFor(name);
  for (const file of files) {
    const err = Attach.checkFile(file, items);
    if (err) {
      showBanner(err);
      continue;
    }
    const item = {
      key: ++nextAttachmentKey,
      file,
      workspace: name,
      name: Attach.nameFor(file),
      size: file.size,
      image: Attach.isImage(file),
      status: "uploading",
      progress: 0,
      preview: Attach.isImage(file) ? URL.createObjectURL(file) : null,
    };
    items.push(item);
    startUpload(item);
  }
  renderAttachments();
}

function startUpload(item) {
  item.status = "uploading";
  item.progress = 0;
  item.error = null;
  const refresh = () => state.selected === item.workspace && renderAttachments();
  item.upload = Attach.upload(item.workspace, item.file, item.name, (p) => {
    // Redraw only when the percentage changes.
    if (Math.round(p * 100) === Math.round(item.progress * 100)) return;
    item.progress = p;
    refresh();
  });
  item.upload.promise.then(
    (att) => {
      item.id = att.id;
      item.status = "done";
      item.file = null;
      refresh();
    },
    (err) => {
      if (err.aborted) return;
      item.status = "error";
      item.error = err.message;
      refresh();
    },
  );
  refresh();
}

function removeAttachment(item) {
  if (item.upload && item.status === "uploading") item.upload.abort();
  if (item.preview) URL.revokeObjectURL(item.preview);
  const items = pendingFor(item.workspace);
  items.splice(items.indexOf(item), 1);
  renderAttachments();
}

// onPaste turns pasted images and files into attachments. Plain text is
// left to the browser.
function onPaste(e) {
  const r = Attach.filesFromTransfer(e.clipboardData, "paste");
  if (!r.handled) return;
  e.preventDefault();
  if (r.message) showBanner(r.message);
  if (r.files.length) addFiles(r.files);
}

function onDrop(e) {
  e.preventDefault();
  $("composer").classList.remove("dragging");
  $("drop-hint").classList.add("hidden");
  const r = Attach.filesFromTransfer(e.dataTransfer, "drop");
  if (r.message) showBanner(r.message);
  if (r.files.length) addFiles(r.files);
}

// ---------- actions ----------

async function sendPrompt() {
  const prompt = $("prompt");
  const text = prompt.value;
  const name = state.selected;
  const items = (name && state.pending.get(name)) || [];
  if ((!text.trim() && items.length === 0) || !state.ws || isWorking() || state.sending) return;
  if (items.some((i) => i.status !== "done")) {
    showBanner("Wait for the attachments to finish uploading, or remove the ones that failed.");
    return;
  }
  state.sending = true;
  render();
  try {
    await api("POST", wsPath(name) + "/chat", { prompt: text, attachments: items.map((i) => i.id) });
    if (state.selected === name) prompt.value = "";
    state.pending.delete(name);
    if (state.selected === name) renderAttachments();
    state.drafts.delete(name);
    hideBanner();
  } catch (e) {
    showBanner(e.status === 409 ? "Claude is still working on the previous message." : "Could not send the message: " + e.message);
  } finally {
    state.sending = false;
    refreshList();
  }
}

// resumeSandbox resumes a stopped sandbox without sending a message, so
// attachments can be opened and Claude's last state is fetched.
async function resumeSandbox() {
  const ws = state.ws;
  if (!ws || state.resuming) return;
  state.resuming = true;
  render();
  try {
    await api("POST", wsPath(ws.name) + "/connect", {});
    hideBanner();
  } catch (e) {
    showBanner("Could not resume the sandbox: " + e.message);
  } finally {
    state.resuming = false;
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
  // Already terminated: this only saves it again, so there is nothing to confirm.
  const retry = isTerminated(ws);
  const msg =
    `Terminate the workspace for ${ws.repo}?\n\n` +
    "This permanently deletes the Vercel Sandbox with its files and snapshots. Unpublished changes are lost, and Claude cannot continue this conversation. " +
    (ws.pull_request ? `The branch and PR #${ws.pull_request.number} stay on GitHub. ` : "Nothing has been published yet. ") +
    "The conversation stays readable in Mosso's history.";
  if (!retry && !confirm(msg)) return;
  const btn = $("terminate");
  btn.disabled = true;
  btn.textContent = retry ? "Saving…" : "Terminating…";
  try {
    // The workspace stays selected: its event stream reports that it is
    // now history.
    await api("DELETE", wsPath(ws.name));
    state.drafts.delete(ws.name);
    state.pending.delete(ws.name);
  } catch (e) {
    showBanner("Could not terminate the workspace: " + e.message + (e.status === 502 ? "\nCheck the Vercel dashboard for a leftover sandbox." : ""));
  } finally {
    btn.disabled = false;
    refreshList(); // renders the button for the workspace's new state
  }
}

// toggleArchive archives the selected terminated workspace, or brings an
// archived one back into the history.
async function toggleArchive() {
  const ws = state.ws;
  if (!ws || state.archiving) return;
  state.archiving = true;
  render();
  try {
    const updated = await api("POST", wsPath(ws.name) + (ws.archived_at ? "/unarchive" : "/archive"), {});
    if (state.selected === updated.name) state.ws = updated;
    hideBanner();
  } catch (e) {
    showBanner(`Could not ${ws.archived_at ? "unarchive" : "archive"} the workspace: ` + e.message);
  } finally {
    state.archiving = false;
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
  if (!state.settings) state.settings = await api("GET", "/settings").catch(() => null);
  if (state.settings) {
    modelOptions($("create-model"), state.settings.default_model);
    $("create-image").value = state.settings.default_image || "";
    $("create-setup").value = state.settings.default_setup_script || "";
  }
  $("create-image").placeholder = imageName("");
  $("create-env").open = false;
  updateEnvSummary();
  $("create-error").classList.add("hidden");
  createSeq++; // a create still running no longer owns the dialog
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

// updateEnvSummary says what the New workspace dialog's sandbox
// environment is while it is collapsed.
function updateEnvSummary() {
  const image = $("create-image").value.trim();
  const setup = $("create-setup").value.trim();
  $("create-env-summary").textContent = "· " + imageName(image) + (setup ? " · setup script" : " · no setup script");
}

function setCreating(on) {
  $("create-submit").disabled = on;
  $("create-submit").textContent = on ? "Creating… (about a minute)" : "Create";
  $("create-repo").disabled = on;
  $("create-base").disabled = on;
  $("create-model").disabled = on;
  $("create-prompt").disabled = on;
  $("create-image").disabled = on;
  $("create-setup").disabled = on;
}

// createSeq identifies the dialog's current use: each create request and
// each opening of the dialog takes the next number. A create request only
// touches the dialog while it is still the current one, so one that ends
// late cannot close, clear or reset a dialog opened since.
let createSeq = 0;

async function submitCreate(ev) {
  ev.preventDefault();
  const repo = $("create-repo").value.trim().replace(/^https:\/\/github\.com\//, "").replace(/\.git$/, "").replace(/\/$/, "");
  const base = $("create-base").value.trim();
  const model = $("create-model").value;
  const prompt = $("create-prompt").value.trim();
  const image = $("create-image").value.trim();
  const setupScript = $("create-setup").value;
  if (!repo) return;
  const err = $("create-error");
  err.classList.add("hidden");
  setCreating(true);
  const mine = ++createSeq;
  const current = () => mine === createSeq;
  const dialog = $("create-dialog");
  // As soon as the server lists the new workspace (provisioning takes a
  // few seconds; cloning and installing take the rest of the minute),
  // close the dialog and open it: the chat shows the first message and
  // "Setting up" until Claude starts.
  const before = new Set(state.workspaces.map((w) => w.name));
  let opened = null;
  const poll = setInterval(async () => {
    await refreshList();
    if (opened) return;
    const fresh = state.workspaces.find((w) => !before.has(w.name) && w.status === "creating" && w.repo.toLowerCase() === repo.toLowerCase());
    if (!fresh) return;
    opened = fresh.name;
    if (!current()) return; // the dialog was opened again meanwhile
    // The server has the first message now; clear it so the next
    // workspace starts empty (it is put back if this create fails).
    $("create-prompt").value = "";
    if (dialog.open) dialog.close();
    select(fresh.name);
  }, 1000);
  try {
    const body = { repo };
    if (base) body.base = base;
    if (model) body.model = model;
    if (prompt) body.prompt = prompt;
    // What the dialog shows, defaults included: empty means none. Without
    // the settings the fields were not prefilled, so the server's
    // defaults apply.
    if (state.settings) {
      body.image = image;
      body.setup_script = setupScript;
    }
    const ws = await api("POST", "/workspaces", body);
    localStorage.setItem("mosso.lastRepo", repo);
    if (!current()) {
      await refreshList(); // listed; the dialog belongs to a newer use
      return;
    }
    $("create-prompt").value = "";
    if (dialog.open) dialog.close();
    await refreshList();
    // Open it, unless the user has moved on to another workspace since.
    if (current() && (!opened || state.selected === opened)) select(ws.name);
  } catch (e) {
    if (current() && dialog.open) {
      err.textContent = e.message;
      err.classList.remove("hidden");
      err.scrollIntoView({ block: "nearest" });
      if (e.status === 503 && /GitHub is not connected/.test(e.message)) {
        dialog.close();
        openGitHub();
      }
      return;
    }
    // The dialog closed early or was opened again: report it in a banner,
    // and put the first message back unless the dialog is in use.
    await refreshList();
    let kept = false;
    if (prompt && !dialog.open) {
      if (!$("create-prompt").value) $("create-prompt").value = prompt;
      kept = $("create-prompt").value === prompt;
    }
    showBanner(`Could not create the ${repo} workspace: ${e.message}` + (kept ? " Your first message is kept in the New workspace dialog." : ""));
  } finally {
    clearInterval(poll);
    if (current()) setCreating(false);
  }
}

// ---------- wiring ----------

function init() {
  $("new-workspace").addEventListener("click", openCreate);
  $("empty-new").addEventListener("click", openCreate);
  $("create-form").addEventListener("submit", submitCreate);
  $("create-cancel").addEventListener("click", () => $("create-dialog").close());
  $("create-prompt").addEventListener("keydown", (e) => {
    if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) {
      e.preventDefault();
      $("create-form").requestSubmit();
    }
  });
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
  $("archive").addEventListener("click", toggleArchive);
  $("resume").addEventListener("click", resumeSandbox);
  $("empty-connect").addEventListener("click", openGitHub);
  $("github-form").addEventListener("submit", submitGitHub);
  $("settings-form").addEventListener("submit", submitSettings);
  $("settings-cancel").addEventListener("click", () => $("settings-dialog").close());
  $("create-image").addEventListener("input", updateEnvSummary);
  $("create-setup").addEventListener("input", updateEnvSummary);
  $("github-cancel").addEventListener("click", closeGitHub);
  $("github-dialog").addEventListener("close", () => ($("github-token").value = ""));
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
  // Paste, drop and the paperclip all feed addFiles.
  document.addEventListener("paste", (e) => {
    const t = e.target;
    if (document.querySelector("dialog[open]") || !state.ws) return;
    if (t !== document.body && t !== $("prompt") && !$("workspace").contains(t)) return;
    onPaste(e);
  });
  $("attach").addEventListener("click", () => $("file-input").click());
  $("file-input").addEventListener("change", (e) => {
    addFiles(Array.from(e.target.files));
    e.target.value = ""; // so choosing the same file again works
  });
  const hasFiles = (e) => Array.from((e.dataTransfer && e.dataTransfer.types) || []).some((t) => t === "Files" || t === "text/uri-list" || t === "text/html");
  let dragDepth = 0;
  // Always stop the browser from navigating to a file dropped anywhere.
  window.addEventListener("dragover", (e) => hasFiles(e) && e.preventDefault());
  window.addEventListener("drop", (e) => hasFiles(e) && e.preventDefault());
  const zone = $("workspace");
  zone.addEventListener("dragenter", (e) => {
    if (!hasFiles(e)) return;
    dragDepth++;
    $("composer").classList.add("dragging");
    $("drop-hint").classList.remove("hidden");
  });
  zone.addEventListener("dragleave", () => {
    if (--dragDepth > 0) return;
    dragDepth = 0;
    $("composer").classList.remove("dragging");
    $("drop-hint").classList.add("hidden");
  });
  zone.addEventListener("drop", (e) => {
    dragDepth = 0;
    if (hasFiles(e)) onDrop(e);
  });
  $("preview-close").addEventListener("click", () => $("preview-dialog").close());
  $("preview-dialog").addEventListener("click", (e) => e.target === $("preview-dialog") && $("preview-dialog").close());
  $("preview-dialog").addEventListener("close", () => $("preview-img").removeAttribute("src"));
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
