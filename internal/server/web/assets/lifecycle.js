// Sandbox lifecycle helpers for the web UI: the Vercel plan, session
// lengths, idle shutdown and workspace statuses. No DOM here, so it can be
// tested with Node (see webtest/lifecycle.test.js). The server enforces
// the same rules (internal/server/plan.go and idle.go); these only decide
// what the UI offers and says.
"use strict";

const Lifecycle = (() => {
  const MIN = 60;
  const HOUR = 3600;
  // Session lengths the New workspace dialog offers, up to the plan's
  // maximum (45 minutes on Hobby, 24 hours on Pro and Enterprise).
  const SESSION_CHOICES = [15 * MIN, 30 * MIN, 45 * MIN, HOUR, 2 * HOUR, 4 * HOUR, 8 * HOUR, 12 * HOUR, 24 * HOUR];
  // Idle timeouts; 0 never stops an idle sandbox.
  const IDLE_CHOICES = [5 * MIN, 15 * MIN, 30 * MIN, HOUR, 0];
  const DEFAULT_SESSION = 45 * MIN;
  const DEFAULT_IDLE = 15 * MIN;
  const HOBBY_MAX = 45 * MIN;

  // duration writes seconds as "45 minutes", "1 hour", "1 hour 30 minutes".
  function duration(seconds) {
    const m = Math.round(seconds / MIN);
    const h = Math.floor(m / 60);
    const rest = m % 60;
    const unit = (n, one) => `${n} ${one}${n === 1 ? "" : "s"}`;
    const parts = [];
    if (h) parts.push(unit(h, "hour"));
    if (rest || !h) parts.push(unit(rest, "minute"));
    return parts.join(" ");
  }

  // maxSession is the longest session the plan allows, in seconds: the
  // Hobby limit until Mosso knows better.
  function maxSession(vercel) {
    return (vercel && vercel.max_session_seconds) || HOBBY_MAX;
  }

  // sessionOptions are the session lengths to offer: those the plan
  // allows, and the selected one if it is not among them.
  function sessionOptions(vercel, selected) {
    const max = maxSession(vercel);
    const values = SESSION_CHOICES.filter((s) => s <= max);
    if (selected && !values.includes(selected) && selected <= max) values.push(selected);
    values.sort((a, b) => a - b);
    return values.map((s) => ({ value: s, label: duration(s) + (s === DEFAULT_SESSION ? " (default)" : "") }));
  }

  // idleOptions are the idle timeouts to offer, with the selected one.
  function idleOptions(selected) {
    const values = IDLE_CHOICES.slice();
    if (selected !== undefined && selected !== null && !values.includes(selected)) values.splice(values.length - 1, 0, selected);
    return values.map((s) => ({ value: s, label: s === 0 ? "Never" : duration(s) + (s === DEFAULT_IDLE ? " (default)" : "") }));
  }

  // planName names the detected plan.
  function planName(vercel) {
    const plan = vercel && vercel.plan;
    if (!plan) return "plan unknown";
    const name = { hobby: "Hobby", pro: "Pro", enterprise: "Enterprise" }[plan];
    return name || plan;
  }

  // planSummary describes the plan and its session limit, for the sidebar.
  function planSummary(vercel) {
    if (!vercel || !vercel.configured) return "";
    let text = planName(vercel) + " · sessions up to " + duration(maxSession(vercel));
    if (!vercel.plan || !vercel.plan_known) text += " (Hobby limits)";
    return text;
  }

  // statusLabel names a workspace's status.
  function statusLabel(ws) {
    switch (ws.status) {
      case "creating":
        return "Setting up";
      case "connecting":
        return "Connecting";
      case "working":
        return "Working";
      case "stopping":
        return "Stopping";
      case "stopped":
        return "Stopped";
      case "resuming":
        return "Resuming";
      case "terminated":
        return ws.archived_at ? "Archived" : "Terminated";
      case "error":
        return ws.sandbox_state === "gone" ? "Gone" : "Error";
      default:
        return "Idle";
    }
  }

  // idleText says when an idle sandbox will be stopped, or "" if it will
  // not be (something runs in it, it is stopped, or idle shutdown is off).
  function idleText(ws, now) {
    if (!ws || !ws.idle_stop_at) return "";
    const left = new Date(ws.idle_stop_at).getTime() - now;
    if (left <= 60000) return "Stops when idle in under a minute";
    return "Stops when idle in " + Math.ceil(left / 60000) + " min";
  }

  // settingsText sums up a workspace's session length and idle timeout.
  function settingsText(ws) {
    const parts = [];
    if (ws.session_seconds) parts.push("Sessions up to " + duration(ws.session_seconds));
    parts.push(ws.idle_timeout_seconds ? "stops after " + duration(ws.idle_timeout_seconds) + " idle" : "never stopped when idle");
    return parts.join(", ");
  }

  return {
    SESSION_CHOICES,
    IDLE_CHOICES,
    DEFAULT_SESSION,
    DEFAULT_IDLE,
    duration,
    maxSession,
    sessionOptions,
    idleOptions,
    planName,
    planSummary,
    statusLabel,
    idleText,
    settingsText,
  };
})();

if (typeof module !== "undefined") module.exports = Lifecycle;
