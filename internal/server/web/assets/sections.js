// Section helpers for the web UI: grouping workspaces for the sidebar and
// working out what a new workspace starts from. No DOM here, so it can be
// tested with Node (see webtest/sections.test.js). The server applies the
// same rules (createWorkspace in internal/server/workspace.go); these only
// decide what the New workspace dialog shows and sends.
"use strict";

const Sections = (() => {
  // The configuration fields a section gives a new workspace, in the order
  // the dialog shows them.
  const FIELDS = ["repo", "base", "model", "image", "setup_script", "instructions"];

  // group splits workspaces for the sidebar. Workspaces with a sandbox are
  // listed under their section (oldest first, as created), or under
  // "Unsectioned" if they have none or their section is not known; every
  // section is listed, even an empty one. Terminated workspaces go to the
  // history and archived ones apart (both newest first), whatever their
  // section: they keep it, and the sidebar names it.
  function group(workspaces, sections) {
    const bySection = new Map((sections || []).map((s) => [s.id, { section: s, workspaces: [] }]));
    const out = { sections: [...bySection.values()], unsectioned: [], history: [], archived: [] };
    for (const w of workspaces || []) {
      if (w.archived_at) out.archived.push(w);
      else if (w.sandbox_state === "terminated") out.history.push(w);
      else {
        const g = w.section_id ? bySection.get(w.section_id) : null;
        (g ? g.workspaces : out.unsectioned).push(w);
      }
    }
    out.history.reverse();
    out.archived.reverse();
    return out;
  }

  // startingPoint is what the New workspace dialog is filled with for a
  // section (null for none): the section's configuration, where a section
  // without a model uses the default model. Without a section, the
  // defaults from Settings and no instructions. An empty image or setup
  // script in a section means the built-in runtime or none, not the
  // defaults.
  function startingPoint(section, settings) {
    const s = settings || {};
    if (!section) {
      return { repo: "", base: "", model: s.default_model || "", image: s.default_image || "", setup_script: s.default_setup_script || "", instructions: "" };
    }
    return {
      repo: section.repo || "",
      base: section.base || "",
      model: section.model || s.default_model || "",
      image: section.image || "",
      setup_script: section.setup_script || "",
      instructions: section.instructions || "",
    };
  }

  // normalize mirrors how the server reads a field, so that a value is only
  // "changed" when the server would see it differently.
  function normalize(field, value) {
    value = value || "";
    if (field === "setup_script") return value.trim() ? value.replace(/\r\n/g, "\n") : "";
    if (field === "instructions") return value.replace(/\r\n/g, "\n").trim();
    return value.trim();
  }

  // changed lists the fields whose values differ from the starting point.
  function changed(start, values) {
    return FIELDS.filter((f) => normalize(f, start[f]) !== normalize(f, values[f]));
  }

  // createBody is the POST /workspaces body for the dialog's values: what
  // it shows, sent explicitly, so the section's or the defaults' values
  // only apply where the dialog shows them. Without a section and without
  // the settings, the image and setup script were not prefilled, so they
  // are left to the server's defaults.
  function createBody(sectionId, values, prefilled) {
    const body = { repo: values.repo };
    if (sectionId) body.section_id = sectionId;
    body.base = values.base || "";
    if (values.model) body.model = values.model;
    if (sectionId || prefilled) {
      body.image = values.image || "";
      body.setup_script = values.setup_script || "";
    }
    body.instructions = values.instructions || "";
    if (values.prompt) body.prompt = values.prompt;
    return body;
  }

  // cleanRepo accepts a GitHub URL or "owner/repo".
  function cleanRepo(text) {
    return (text || "")
      .trim()
      .replace(/^https:\/\/github\.com\//, "")
      .replace(/\.git$/, "")
      .replace(/\/$/, "");
  }

  return { FIELDS, group, startingPoint, changed, createBody, cleanRepo };
})();

if (typeof module !== "undefined") module.exports = Sections;
