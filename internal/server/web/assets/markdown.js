// A small Markdown renderer for Claude's replies. It builds DOM nodes and
// only ever sets text through textContent, so Claude's output (which may
// echo prompt-injected content) can never inject HTML or scripts. Links are
// limited to http(s) URLs.
//
// Supported: paragraphs, headings, fenced code, lists, block quotes, tables,
// horizontal rules, `code`, **bold**, *italic*, [links](https://…) and bare
// URLs. Anything else is shown as plain text.
"use strict";

(function () {
  function el(tag, cls) {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    return e;
  }

  function safeURL(url) {
    return /^https?:\/\//i.test(url) ? url : null;
  }

  function link(href, text) {
    const a = el("a");
    a.href = href;
    a.target = "_blank";
    a.rel = "noopener noreferrer";
    a.textContent = text;
    return a;
  }

  const inlinePattern = new RegExp(
    [
      "(`+)([\\s\\S]*?[^`])\\1(?!`)", // 1,2: code span
      "\\[([^\\]\\n]+)\\]\\(([^)\\s]+)\\)", // 3,4: link
      "\\*\\*([^*\\n][\\s\\S]*?)\\*\\*", // 5: bold
      "__([^_\\n][\\s\\S]*?)__", // 6: bold
      "\\*([^*\\s](?:[^*\\n]*[^*\\s])?)\\*", // 7: italic
      "(?<![\\w])_([^_\\s](?:[^_\\n]*[^_\\s])?)_(?![\\w])", // 8: italic
      "(https?:\\/\\/[^\\s<>()\\[\\]]*[^\\s<>()\\[\\].,;:!?'\"])", // 9: bare URL
    ].join("|"),
    "g",
  );

  function inline(text, parent) {
    let last = 0;
    inlinePattern.lastIndex = 0;
    const re = new RegExp(inlinePattern.source, "g");
    let m;
    while ((m = re.exec(text)) !== null) {
      if (m.index > last) parent.appendChild(document.createTextNode(text.slice(last, m.index)));
      if (m[2] !== undefined) {
        const c = el("code");
        c.textContent = m[2].replace(/^ (.*) $/, "$1");
        parent.appendChild(c);
      } else if (m[3] !== undefined) {
        const href = safeURL(m[4]);
        if (href) {
          const a = link(href, "");
          inline(m[3], a);
          parent.appendChild(a);
        } else {
          parent.appendChild(document.createTextNode(m[0]));
        }
      } else if (m[5] !== undefined || m[6] !== undefined) {
        const b = el("strong");
        inline(m[5] !== undefined ? m[5] : m[6], b);
        parent.appendChild(b);
      } else if (m[7] !== undefined || m[8] !== undefined) {
        const i = el("em");
        inline(m[7] !== undefined ? m[7] : m[8], i);
        parent.appendChild(i);
      } else if (m[9] !== undefined) {
        parent.appendChild(link(m[9], m[9]));
      }
      last = re.lastIndex;
    }
    if (last < text.length) parent.appendChild(document.createTextNode(text.slice(last)));
  }

  const fence = /^\s*(```+|~~~+)\s*([\w+#.-]*)\s*$/;
  const heading = /^(#{1,6})\s+(.*?)\s*#*\s*$/;
  const listItem = /^(\s*)([-*+]|\d{1,9}[.)])\s+(.*)$/;
  const rule = /^\s*([-*_])(\s*\1){2,}\s*$/;
  const tableSep = /^\s*\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)*\|?\s*$/;

  function splitRow(line) {
    let s = line.trim();
    if (s.startsWith("|")) s = s.slice(1);
    if (s.endsWith("|") && !s.endsWith("\\|")) s = s.slice(0, -1);
    return s.split(/(?<!\\)\|/).map((c) => c.trim().replace(/\\\|/g, "|"));
  }

  function render(text) {
    const root = document.createDocumentFragment();
    const lines = String(text).replace(/\r\n?/g, "\n").split("\n");
    let i = 0;
    while (i < lines.length) {
      const line = lines[i];
      if (line.trim() === "") {
        i++;
        continue;
      }

      let m = line.match(fence);
      if (m) {
        const close = m[1];
        const body = [];
        i++;
        while (i < lines.length && !(lines[i].trim().startsWith(close[0].repeat(close.length)) && lines[i].trim().replace(/[`~]/g, "") === "")) {
          body.push(lines[i]);
          i++;
        }
        i++; // closing fence
        const pre = el("pre");
        const code = el("code");
        if (m[2]) code.dataset.lang = m[2];
        code.textContent = body.join("\n");
        pre.appendChild(code);
        root.appendChild(pre);
        continue;
      }

      m = line.match(heading);
      if (m) {
        const h = el("h" + Math.min(m[1].length + 2, 6));
        inline(m[2], h);
        root.appendChild(h);
        i++;
        continue;
      }

      if (rule.test(line)) {
        root.appendChild(el("hr"));
        i++;
        continue;
      }

      if (line.includes("|") && i + 1 < lines.length && tableSep.test(lines[i + 1])) {
        const table = el("table");
        const head = el("thead");
        const tr = el("tr");
        for (const cell of splitRow(line)) {
          const th = el("th");
          inline(cell, th);
          tr.appendChild(th);
        }
        head.appendChild(tr);
        table.appendChild(head);
        const body = el("tbody");
        i += 2;
        while (i < lines.length && lines[i].includes("|") && lines[i].trim() !== "") {
          const row = el("tr");
          for (const cell of splitRow(lines[i])) {
            const td = el("td");
            inline(cell, td);
            row.appendChild(td);
          }
          body.appendChild(row);
          i++;
        }
        table.appendChild(body);
        const wrap = el("div", "table-wrap");
        wrap.appendChild(table);
        root.appendChild(wrap);
        continue;
      }

      if (/^\s*>/.test(line)) {
        const quoted = [];
        while (i < lines.length && /^\s*>/.test(lines[i])) {
          quoted.push(lines[i].replace(/^\s*>\s?/, ""));
          i++;
        }
        const bq = el("blockquote");
        bq.appendChild(render(quoted.join("\n")));
        root.appendChild(bq);
        continue;
      }

      m = line.match(listItem);
      if (m) {
        root.appendChild(renderList(lines, i, (next) => (i = next)));
        continue;
      }

      // Paragraph: consecutive lines that do not start another block.
      const para = [];
      while (
        i < lines.length &&
        lines[i].trim() !== "" &&
        !fence.test(lines[i]) &&
        !heading.test(lines[i]) &&
        !listItem.test(lines[i]) &&
        !/^\s*>/.test(lines[i]) &&
        !rule.test(lines[i])
      ) {
        para.push(lines[i].trim());
        i++;
      }
      const p = el("p");
      inline(para.join("\n"), p);
      root.appendChild(p);
    }
    return root;
  }

  // renderList renders a (possibly nested) list starting at lines[start].
  function renderList(lines, start, setIndex) {
    const first = lines[start].match(listItem);
    const indent = first[1].length;
    const ordered = /\d/.test(first[2]);
    const list = el(ordered ? "ol" : "ul");
    if (ordered) {
      const n = parseInt(first[2], 10);
      if (n !== 1) list.start = n;
    }
    let i = start;
    let item = null;
    while (i < lines.length) {
      const line = lines[i];
      const m = line.match(listItem);
      if (m && m[1].length === indent) {
        item = el("li");
        const task = m[3].match(/^\[([ xX])\]\s+(.*)$/);
        if (task) {
          const box = el("input");
          box.type = "checkbox";
          box.disabled = true;
          box.checked = task[1] !== " ";
          item.appendChild(box);
          item.appendChild(document.createTextNode(" "));
          inline(task[2], item);
        } else {
          inline(m[3], item);
        }
        list.appendChild(item);
        i++;
      } else if (m && m[1].length > indent && item) {
        item.appendChild(renderList(lines, i, (next) => (i = next)));
      } else if (line.trim() !== "" && /^\s+/.test(line) && item && !m) {
        item.appendChild(document.createTextNode(" "));
        inline(line.trim(), item);
        i++;
      } else if (line.trim() === "" && i + 1 < lines.length && (lines[i + 1].match(listItem) || [])[1]?.length >= indent) {
        i++; // blank line between items
      } else {
        break;
      }
    }
    setIndex(i);
    return list;
  }

  window.renderMarkdown = render;
})();
