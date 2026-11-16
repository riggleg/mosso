// Run with: node --test internal/server/webtest/*.test.js
"use strict";
const test = require("node:test");
const assert = require("node:assert");
const Sections = require("../web/assets/sections.js");

const settings = { default_model: "claude-opus-5-5", default_image: "dev-image:v1", default_setup_script: "make deps\n" };
const perx = { id: 1, name: "Perx API", repo: "perx/api", base: "master", model: "claude-sonnet-5-5", image: "ruby:v3", setup_script: "bundle install\n", instructions: "PRs to master and uat." };
const layer6 = { id: 2, name: "Perx API Layer 6", repo: "perx/api", base: "layer6", model: "", image: "", setup_script: "", instructions: "" };
const ws = (name, extra) => ({ name, sandbox_state: "running", ...extra });

test("workspaces are grouped under their sections, in order", () => {
  const g = Sections.group(
    [ws("a", { section_id: 1 }), ws("b"), ws("c", { section_id: 2 }), ws("d", { section_id: 1 })],
    [perx, layer6],
  );
  assert.deepStrictEqual(
    g.sections.map((s) => [s.section.name, s.workspaces.map((w) => w.name)]),
    [
      ["Perx API", ["a", "d"]],
      ["Perx API Layer 6", ["c"]],
    ],
  );
  assert.deepStrictEqual(g.unsectioned.map((w) => w.name), ["b"]);
});

test("empty sections are listed; unknown sections fall back to Unsectioned", () => {
  const g = Sections.group([ws("a", { section_id: 9 })], [perx]);
  assert.strictEqual(g.sections.length, 1);
  assert.deepStrictEqual(g.sections[0].workspaces, []);
  assert.deepStrictEqual(g.unsectioned.map((w) => w.name), ["a"]);
});

test("history and archived workspaces stay apart, newest first, keeping their section", () => {
  const g = Sections.group(
    [
      ws("old", { sandbox_state: "terminated", section_id: 1 }),
      ws("archived", { sandbox_state: "terminated", archived_at: "2026-05-01T00:00:00Z", section_id: 1 }),
      ws("new", { sandbox_state: "terminated" }),
    ],
    [perx],
  );
  assert.deepStrictEqual(g.history.map((w) => w.name), ["new", "old"]);
  assert.deepStrictEqual(g.archived.map((w) => w.name), ["archived"]);
  assert.strictEqual(g.history[1].section_id, 1);
  assert.deepStrictEqual(g.sections[0].workspaces, []);
});

test("a section's configuration is the starting point", () => {
  assert.deepStrictEqual(Sections.startingPoint(perx, settings), {
    repo: "perx/api",
    base: "master",
    model: "claude-sonnet-5-5",
    image: "ruby:v3",
    setup_script: "bundle install\n",
    instructions: "PRs to master and uat.",
  });
});

test("a section without a model uses the default model, but not the default environment", () => {
  const start = Sections.startingPoint(layer6, settings);
  assert.strictEqual(start.model, "claude-opus-5-5");
  assert.strictEqual(start.image, "");
  assert.strictEqual(start.setup_script, "");
  assert.strictEqual(start.instructions, "");
});

test("without a section, the defaults from Settings", () => {
  assert.deepStrictEqual(Sections.startingPoint(null, settings), {
    repo: "",
    base: "",
    model: "claude-opus-5-5",
    image: "dev-image:v1",
    setup_script: "make deps\n",
    instructions: "",
  });
  assert.strictEqual(Sections.startingPoint(null, null).model, "");
});

test("overrides are the fields that differ, ignoring what the server ignores", () => {
  const start = Sections.startingPoint(perx, settings);
  assert.deepStrictEqual(Sections.changed(start, { ...start }), []);
  assert.deepStrictEqual(Sections.changed(start, { ...start, repo: " perx/api ", instructions: "\r\nPRs to master and uat.\n" }), []);
  assert.deepStrictEqual(Sections.changed(start, { ...start, base: "uat", model: "claude-haiku-5-5", setup_script: "  " }), ["base", "model", "setup_script"]);
});

test("the create request sends what the dialog shows", () => {
  const values = { ...Sections.startingPoint(perx, settings), base: "", prompt: "Fix the bug" };
  assert.deepStrictEqual(Sections.createBody(1, values, true), {
    repo: "perx/api",
    section_id: 1,
    base: "",
    model: "claude-sonnet-5-5",
    image: "ruby:v3",
    setup_script: "bundle install\n",
    instructions: "PRs to master and uat.",
    prompt: "Fix the bug",
  });
});

test("without a section or settings, the server's defaults apply", () => {
  const body = Sections.createBody(null, { repo: "octo/widgets", base: "", model: "", image: "", setup_script: "", instructions: "", prompt: "" }, false);
  assert.deepStrictEqual(body, { repo: "octo/widgets", base: "", instructions: "" });
});

test("repositories can be pasted as URLs", () => {
  assert.strictEqual(Sections.cleanRepo(" https://github.com/perx/api.git "), "perx/api");
  assert.strictEqual(Sections.cleanRepo("perx/api/"), "perx/api");
});
