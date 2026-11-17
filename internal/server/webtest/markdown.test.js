// Run with: node --test internal/server/webtest/*.test.js
"use strict";
const test = require("node:test");
const assert = require("node:assert");

// A minimal DOM: enough for markdown.js, which only creates elements, text
// nodes and fragments and sets properties on them.
class Node {
  constructor(tag) {
    this.tagName = tag;
    this.childNodes = [];
    this.dataset = {};
    this.attrs = {};
  }
  appendChild(n) {
    if (n.tagName === "#fragment") this.childNodes.push(...n.childNodes);
    else this.childNodes.push(n);
    return n;
  }
  set textContent(t) {
    this.childNodes = [text(t)];
  }
  get textContent() {
    return this.childNodes.map((c) => c.textContent).join("");
  }
  // find returns the descendants with tag.
  find(tag) {
    const out = [];
    for (const c of this.childNodes) {
      if (c.tagName === tag) out.push(c);
      if (c.find) out.push(...c.find(tag));
    }
    return out;
  }
}
const text = (t) => ({ tagName: "#text", textContent: String(t) });
global.document = {
  createElement: (tag) => new Node(tag),
  createDocumentFragment: () => new Node("#fragment"),
  createTextNode: text,
};
global.window = {};
require("../web/assets/markdown.js");
const render = (md, opts) => window.renderMarkdown(md, opts);

test("an image the caller resolves is shown with the caller's node", () => {
  const seen = [];
  const image = (ref, alt) => {
    seen.push([ref, alt]);
    const n = new Node("mosso-image");
    n.ref = ref;
    return n;
  };
  const root = render('Here: ![Home page](screenshots/home.png) and ![chart](<out/my chart.svg> "Chart").', { image });
  assert.deepStrictEqual(seen, [
    ["screenshots/home.png", "Home page"],
    ["out/my chart.svg", "chart"],
  ]);
  const shown = root.find("mosso-image");
  assert.strictEqual(shown.length, 2);
  assert.strictEqual(root.find("img").length, 0, "markdown.js never creates an img itself");
  assert.match(root.textContent, /^Here:  and \.$/);
});

test("unresolved images never load their reference", () => {
  const cases = {
    "![x](javascript:void)": "[image: x]",
    "![x](data:image/svg+xml;base64,PHN2Zz4=)": "[image: x]",
    "![x](file:///etc/passwd)": "[image: x]",
    "![](missing.png)": "[image]",
  };
  for (const [md, shown] of Object.entries(cases)) {
    const root = render(md, { image: () => null });
    assert.strictEqual(root.find("img").length, 0, md);
    assert.strictEqual(root.find("a").length, 0, md);
    assert.strictEqual(root.textContent, shown, md);
  }
});

test("a remote image becomes a link, not an image load", () => {
  const root = render("![logo](https://example.com/logo.png)");
  const links = root.find("a");
  assert.strictEqual(links.length, 1);
  assert.strictEqual(links[0].href, "https://example.com/logo.png");
  assert.strictEqual(links[0].rel, "noopener noreferrer");
  assert.strictEqual(root.find("img").length, 0);
});

test("alt text and references are text, never markup", () => {
  let got;
  render('![<img src=x onerror=alert(1)>](a.png)', { image: (ref, alt) => ((got = alt), null) });
  assert.strictEqual(got, "<img src=x onerror=alert(1)>");
  const root = render('![<b>bold</b>](<script>.png)');
  assert.strictEqual(root.find("b").length + root.find("script").length + root.find("img").length, 0);
});

test("links and other inline syntax still work next to images", () => {
  const root = render("[docs](https://example.com) ![s](s.png) **bold** `![not](an image)`", { image: () => new Node("mosso-image") });
  assert.strictEqual(root.find("a").length, 1);
  assert.strictEqual(root.find("mosso-image").length, 1);
  assert.strictEqual(root.find("strong").length, 1);
  assert.strictEqual(root.find("code")[0].textContent, "![not](an image)");
});

test("images inside lists, quotes and tables are resolved too", () => {
  const refs = [];
  render("- ![a](a.png)\n\n> ![b](b.png)\n\n| x |\n| -- |\n| ![c](c.png) |", { image: (ref) => (refs.push(ref), null) });
  assert.deepStrictEqual(refs, ["a.png", "b.png", "c.png"]);
});
