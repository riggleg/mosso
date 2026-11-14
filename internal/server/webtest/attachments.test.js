// Run with: node --test internal/server/webtest
"use strict";
const test = require("node:test");
const assert = require("node:assert");
const Attach = require("../web/assets/attachments.js");

const file = (name, type, size = 10) => ({ name, type, size });
// transfer builds a DataTransfer-like object.
const transfer = ({ files = [], items, types = [], text = "" } = {}) => ({
  files,
  items: items || files.map((f) => ({ kind: "file", getAsFile: () => f })),
  types: [...types, ...(files.length ? ["Files"] : [])],
  getData: (t) => (t === "text/plain" ? text : ""),
});

test("a pasted screenshot becomes an attachment", () => {
  const shot = file("image.png", "image/png");
  const r = Attach.filesFromTransfer(transfer({ files: [shot] }));
  assert.deepStrictEqual(r.files, [shot]);
  assert.ok(r.handled);
});

test("an image wins over its text and HTML representations", () => {
  const img = file("image.png", "image/png");
  const r = Attach.filesFromTransfer(transfer({ files: [img], types: ["text/plain", "text/html"], text: "https://example.com/a.png" }));
  assert.strictEqual(r.files.length, 1);
  assert.ok(r.handled, "the paste must be intercepted so no URL is inserted");
});

test("files exposed only as items are found", () => {
  const f = file("a.pdf", "application/pdf");
  const dt = { files: [], items: [{ kind: "file", getAsFile: () => f }, { kind: "string", getAsFile: () => null }], types: ["Files"], getData: () => "" };
  assert.deepStrictEqual(Attach.filesFromTransfer(dt).files, [f]);
});

test("several pasted files all come through", () => {
  const r = Attach.filesFromTransfer(transfer({ files: [file("a.txt", "text/plain"), file("b.png", "image/png")] }));
  assert.strictEqual(r.files.length, 2);
});

test("plain text paste is left alone", () => {
  for (const dt of [transfer({ types: ["text/plain"], text: "hello" }), transfer({ types: ["text/plain", "text/html"], text: "hi" }), transfer({})]) {
    const r = Attach.filesFromTransfer(dt);
    assert.strictEqual(r.handled, false);
    assert.deepStrictEqual(r.files, []);
  }
});

test("a copied file the browser cannot read gets an explanation", () => {
  const r = Attach.filesFromTransfer({ files: [], items: [{ kind: "file", getAsFile: () => null }], types: ["Files", "text/plain"], getData: () => "photo.heic" });
  assert.ok(r.handled);
  assert.match(r.message, /did not give Mosso access/);
});

test("unknown clipboard content is reported, not ignored", () => {
  const r = Attach.filesFromTransfer(transfer({ types: ["application/x-weird"] }));
  assert.ok(r.handled);
  assert.match(r.message, /can't read that clipboard content/);
});

test("a dropped file works; an image dragged from another page is explained", () => {
  const f = file("a.png", "image/png");
  assert.deepStrictEqual(Attach.filesFromTransfer(transfer({ files: [f] }), "drop").files, [f]);
  const r = Attach.filesFromTransfer(transfer({ types: ["text/uri-list", "text/html"] }), "drop");
  assert.deepStrictEqual(r.files, []);
  assert.match(r.message, /Copy Image/);
});

test("checkFile enforces size, count and total limits", () => {
  assert.match(Attach.checkFile(file("e", "", 0), []), /empty/);
  assert.match(Attach.checkFile(file("big.bin", "", Attach.MAX_FILE + 1), []), /per file/);
  assert.strictEqual(Attach.checkFile(file("ok.bin", "", Attach.MAX_FILE), []), null);
  const ten = Array.from({ length: Attach.MAX_FILES }, () => ({ size: 1 }));
  assert.match(Attach.checkFile(file("x", "", 1), ten), /at most 10/);
  assert.match(Attach.checkFile(file("x", "", Attach.MAX_FILE), [{ size: Attach.MAX_TOTAL - 1 }]), /in total/);
});

test("images are recognised by type, and SVG is not one", () => {
  for (const t of ["image/png", "image/jpeg", "image/webp", "image/gif"]) assert.ok(Attach.isImage(file("x", t)));
  assert.ok(!Attach.isImage(file("x.svg", "image/svg+xml")));
  assert.ok(!Attach.isImage(file("x.pdf", "application/pdf")));
});

test("names and sizes", () => {
  const now = new Date(2026, 10, 14, 9, 41, 2);
  assert.strictEqual(Attach.nameFor(file("image.png"), now), "Pasted image 2026-11-14 09.41.02.png");
  assert.strictEqual(Attach.nameFor(file("report.pdf"), now), "report.pdf");
  assert.strictEqual(Attach.extension("main.go"), "GO");
  assert.strictEqual(Attach.extension("Makefile"), "FILE");
  assert.strictEqual(Attach.formatSize(512), "512 B");
  assert.strictEqual(Attach.formatSize(2048), "2.0 KB");
  assert.strictEqual(Attach.formatSize(5 * 1024 * 1024), "5.0 MB");
});

// fakeXHR records a request and lets the test finish it.
function fakeXHR() {
  const x = { upload: {}, open(m, u) { x.method = m; x.url = u; }, send(f) { x.form = f; } };
  return x;
}

test("upload reports progress and resolves with the attachment", async () => {
  const x = fakeXHR();
  const seen = [];
  global.FormData = class { constructor() { this.parts = []; } append(k, v, n) { this.parts.push([k, n]); } };
  const up = Attach.upload("mosso-1", {}, "a.png", (p) => seen.push(p), () => x);
  assert.strictEqual(x.url, "/workspaces/mosso-1/attachments");
  assert.deepStrictEqual(x.form.parts, [["files", "a.png"]]);
  x.upload.onprogress({ lengthComputable: true, loaded: 5, total: 10 });
  x.status = 201;
  x.responseText = JSON.stringify({ attachments: [{ id: "abc", name: "a.png" }] });
  x.onload();
  assert.deepStrictEqual(seen, [0.5]);
  assert.strictEqual((await up.promise).id, "abc");
});

test("upload failures carry the server's message and can be retried", async () => {
  global.FormData = class { append() {} };
  let x = fakeXHR();
  let up = Attach.upload("w", {}, "a", () => {}, () => x);
  x.status = 413;
  x.responseText = '{"error":"a is larger than 25 MiB"}';
  x.onload();
  await assert.rejects(up.promise, /larger than 25 MiB/);

  x = fakeXHR();
  up = Attach.upload("w", {}, "a", () => {}, () => x);
  x.onerror();
  await assert.rejects(up.promise, /Cannot reach the Mosso server/);

  x = fakeXHR(); // retry with a fresh request succeeds
  up = Attach.upload("w", {}, "a", () => {}, () => x);
  x.status = 201;
  x.responseText = '{"attachments":[{"id":"z"}]}';
  x.onload();
  assert.strictEqual((await up.promise).id, "z");

  x = fakeXHR();
  up = Attach.upload("w", {}, "a", () => {}, () => x);
  x.abort = () => x.onabort();
  up.abort();
  await assert.rejects(up.promise, (e) => e.aborted === true);
});
