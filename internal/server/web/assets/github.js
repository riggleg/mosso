// GitHub helpers for the web UI: the workspace's pull requests for the
// header, the agent's GitHub operations in the chat, and how Claude's calls
// of Mosso's tools are labelled. No DOM here, so it can be tested with Node
// (see webtest/github.test.js).
"use strict";

const GitHub = (() => {
  // MCP_PREFIX starts the names Claude Code gives Mosso's tools.
  const MCP_PREFIX = "mcp__mosso__";

  // pullRequests lists a workspace's pull requests for the header: every
  // one Mosso knows (opened with Create PR or by Claude), or, for a
  // workspace saved before Mosso kept a list, the one it published. The
  // base branch is named when a pull request does not target the
  // workspace's base, or when there are several.
  function pullRequests(ws) {
    if (!ws) return [];
    let list = ws.pull_requests || [];
    if (!list.length && ws.pull_request) list = [{ ...ws.pull_request, head: ws.branch, base: ws.base, by: "user" }];
    const several = list.length > 1;
    return list.map((pr) => ({
      number: pr.number,
      url: pr.url,
      label: `PR #${pr.number}` + (several || (pr.base && pr.base !== ws.base) ? ` → ${pr.base}` : "") + (pr.draft ? " (draft)" : "") + " ↗",
      title:
        (pr.title ? pr.title + "\n" : "") +
        `${pr.head} → ${pr.base}` +
        (pr.by === "agent" ? "\nOpened by Claude with Mosso's GitHub tools" : pr.by === "user" ? "\nOpened with Create PR" : ""),
    }));
  }

  // action describes a "github" chat entry: an operation Claude had Mosso
  // run on GitHub, or one that failed. The detail is left out when the
  // heading already says it.
  function action(a) {
    const out = { ok: !a.error, heading: a.text, detail: "", link: null, error: a.error || "" };
    const pr = a.pull_request;
    if (pr) {
      out.link = { url: pr.url, text: `#${pr.number} ↗` };
      out.detail = `${pr.head} → ${pr.base}`;
    } else if (a.branch) {
      out.detail = a.branch;
    }
    if (out.detail && (a.text || "").includes(out.detail)) out.detail = "";
    return out;
  }

  // toolName labels a tool Claude called: Mosso's tools by their own name.
  function toolName(name) {
    return name && name.startsWith(MCP_PREFIX) ? "GitHub · " + name.slice(MCP_PREFIX.length).replace(/^github_/, "") : name;
  }

  // toolSummary is a one-line summary of a call of one of Mosso's tools,
  // or "" for other tools.
  function toolSummary(name, input) {
    if (!name || !name.startsWith(MCP_PREFIX)) return "";
    input = input || {};
    switch (name.slice(MCP_PREFIX.length)) {
      case "github_push":
        return `${input.source || "HEAD"} → ${input.branch || "working branch"}`;
      case "github_create_pr":
        return `${input.head || "working branch"} → ${input.base || "base"}: ${input.title || ""}`;
      case "github_get_pr":
      case "github_update_pr":
        return input.number ? `#${input.number}` : "";
      case "github_checks":
        return input.pr ? `#${input.pr}` : input.branch || "";
      case "github_fetch":
        return input.branch ? `origin/${input.branch}` : "";
      case "github_list_prs":
        return input.state || "all";
    }
    return "";
  }

  return { pullRequests, action, toolName, toolSummary };
})();

if (typeof module !== "undefined") module.exports = GitHub;
