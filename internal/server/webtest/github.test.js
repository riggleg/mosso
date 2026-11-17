// Run with: node --test internal/server/webtest/*.test.js
"use strict";
const test = require("node:test");
const assert = require("node:assert");
const GitHub = require("../web/assets/github.js");

const ws = { base: "master", branch: "mosso/0a1b2c3d4e5f" };

test("the header lists every pull request, naming bases when there are several", () => {
  const prs = GitHub.pullRequests({
    ...ws,
    pull_request: { number: 12, url: "https://github.com/o/r/pull/12" },
    pull_requests: [
      { number: 12, url: "https://github.com/o/r/pull/12", head: ws.branch, base: "master", title: "Fix", by: "agent" },
      { number: 13, url: "https://github.com/o/r/pull/13", head: ws.branch + "-uat", base: "uat", draft: true, by: "agent" },
    ],
  });
  assert.deepStrictEqual(
    prs.map((p) => [p.number, p.label]),
    [
      [12, "PR #12 → master ↗"],
      [13, "PR #13 → uat (draft) ↗"],
    ],
  );
  assert.match(prs[0].title, /^Fix\nmosso\/0a1b2c3d4e5f → master\nOpened by Claude/);
});

test("one pull request into the base is just its number", () => {
  const one = GitHub.pullRequests({ ...ws, pull_requests: [{ number: 7, url: "u", head: ws.branch, base: "master", by: "user" }] });
  assert.strictEqual(one[0].label, "PR #7 ↗");
  assert.match(one[0].title, /Opened with Create PR/);
  // Into another base, the base is named.
  const other = GitHub.pullRequests({ ...ws, pull_requests: [{ number: 8, url: "u", head: ws.branch + "-uat", base: "uat" }] });
  assert.strictEqual(other[0].label, "PR #8 → uat ↗");
});

test("workspaces saved before the list still show their pull request", () => {
  const prs = GitHub.pullRequests({ ...ws, pull_request: { number: 3, url: "https://github.com/o/r/pull/3" } });
  assert.deepStrictEqual(prs.map((p) => [p.label, p.url]), [["PR #3 ↗", "https://github.com/o/r/pull/3"]]);
  assert.deepStrictEqual(GitHub.pullRequests({ ...ws }), []);
  assert.deepStrictEqual(GitHub.pullRequests(null), []);
});

test("the agent's GitHub operations show what changed, or the error", () => {
  const opened = GitHub.action({
    tool: "github_create_pr",
    text: "Opened pull request #13: mosso/x-uat → uat",
    pull_request: { number: 13, url: "https://github.com/o/r/pull/13", head: "mosso/x-uat", base: "uat" },
  });
  assert.deepStrictEqual(opened, {
    ok: true,
    heading: "Opened pull request #13: mosso/x-uat → uat",
    detail: "",
    link: { url: "https://github.com/o/r/pull/13", text: "#13 ↗" },
    error: "",
  });
  const updated = GitHub.action({ tool: "github_update_pr", text: "Updated pull request #13: title", pull_request: { number: 13, url: "u", head: "mosso/x-uat", base: "uat" } });
  assert.strictEqual(updated.detail, "mosso/x-uat → uat");
  const pushed = GitHub.action({ tool: "github_push", text: "Pushed 4444444 to mosso/x", branch: "mosso/x", commit: "4444444444444444444444444444444444444444" });
  assert.strictEqual(pushed.detail, "");
  const failed = GitHub.action({ tool: "github_push", text: "Push failed", error: "push: the push is not a fast-forward" });
  assert.strictEqual(failed.ok, false);
  assert.strictEqual(failed.error, "push: the push is not a fast-forward");
  assert.strictEqual(failed.link, null);
});

test("Claude's calls of Mosso's tools are labelled", () => {
  assert.strictEqual(GitHub.toolName("mcp__mosso__github_create_pr"), "GitHub · create_pr");
  assert.strictEqual(GitHub.toolName("Bash"), "Bash");
  assert.strictEqual(GitHub.toolSummary("mcp__mosso__github_create_pr", { head: "mosso/x-uat", base: "uat", title: "Fix" }), "mosso/x-uat → uat: Fix");
  assert.strictEqual(GitHub.toolSummary("mcp__mosso__github_push", {}), "HEAD → working branch");
  assert.strictEqual(GitHub.toolSummary("mcp__mosso__github_checks", { pr: 4 }), "#4");
  assert.strictEqual(GitHub.toolSummary("mcp__mosso__github_fetch", { branch: "uat" }), "origin/uat");
  assert.strictEqual(GitHub.toolSummary("Bash", { command: "ls" }), "");
});
