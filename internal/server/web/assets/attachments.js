// Attachment helpers for the chat composer: reading files out of paste and
// drop events, checking them, and uploading them. No DOM building here, so
// it can be tested with Node (see attachments.test.js). The server checks
// everything again; these checks only give quick, friendly errors.
"use strict";

const Attach = (() => {
  // Keep in step with the limits in internal/server/attachments.go.
  const MAX_FILE = 25 << 20;
  const MAX_TOTAL = 40 << 20;
  const MAX_FILES = 10;
  const IMAGE_TYPES = ["image/png", "image/jpeg", "image/webp", "image/gif"];

  function formatSize(n) {
    if (n < 1024) return n + " B";
    if (n < 1024 * 1024) return (n / 1024).toFixed(n < 10 * 1024 ? 1 : 0) + " KB";
    return (n / 1024 / 1024).toFixed(1) + " MB";
  }

  // Whether the browser may show the file inline. SVG is deliberately not
  // one: it can carry script.
  const isImage = (file) => IMAGE_TYPES.includes(file.type);

  // A short label for a file's icon: its extension, or "FILE".
  function extension(name) {
    const m = /\.([a-z0-9]{1,4})$/i.exec(name || "");
    return m ? m[1].toUpperCase() : "FILE";
  }

  // Pasted screenshots and images are all named "image.png"; give them a name
  // that tells them apart.
  function nameFor(file, now = new Date()) {
    if (!/^image\.[a-z0-9]+$/i.test(file.name || "")) return file.name || "file";
    const p = (n) => String(n).padStart(2, "0");
    const stamp = `${now.getFullYear()}-${p(now.getMonth() + 1)}-${p(now.getDate())} ${p(now.getHours())}.${p(now.getMinutes())}.${p(now.getSeconds())}`;
    return `Pasted image ${stamp}${file.name.slice(file.name.lastIndexOf("."))}`;
  }

  // filesFromTransfer reads the files out of a paste or drop's DataTransfer.
  // It returns {files, message, handled}: handled is false for a plain text
  // paste, which the browser should insert as usual. When the clipboard has
  // an image and text (a copied image also carries its URL or HTML), the
  // image wins and the text is dropped.
  function filesFromTransfer(dt, kind = "paste") {
    if (!dt) return { files: [], handled: false };
    let files = Array.from(dt.files || []);
    if (files.length === 0) {
      for (const item of Array.from(dt.items || [])) {
        if (item.kind !== "file") continue;
        const f = item.getAsFile();
        if (f) files.push(f);
      }
    }
    if (files.length > 0) return { files, handled: true };

    const types = Array.from(dt.types || []);
    const hasText = types.includes("text/plain") && dt.getData("text/plain") !== "";
    if (kind === "paste") {
      // Finder lists a copied file as "Files", but the browser may not
      // expose its contents.
      if (types.includes("Files")) {
        return { files: [], handled: true, message: "Your browser did not give Mosso access to the copied file. Drag it into the composer, or use the paperclip button." };
      }
      if (hasText || types.length === 0) return { files: [], handled: false };
      if (types.includes("text/html") || types.includes("text/uri-list")) return { files: [], handled: false };
      return { files: [], handled: true, message: "Mosso can't read that clipboard content (" + types.join(", ") + "). Paste an image or text, or use the paperclip button." };
    }
    // A drop. An image dragged from another web page arrives as a URL,
    // which the page's security policy does not let Mosso download.
    if (types.includes("text/uri-list") || types.includes("text/html")) {
      return { files: [], handled: true, message: "Mosso can't download images from other pages. Right-click the image, choose Copy Image, and press Cmd+V here, or save it and drag the file in." };
    }
    return { files: [], handled: !hasText, message: hasText ? undefined : "Nothing in that drop could be attached." };
  }

  // checkFile returns an error message, or null if the file may be attached
  // next to the files already in the message.
  function checkFile(file, existing) {
    if (file.size === 0) return `${file.name} is empty.`;
    if (file.size > MAX_FILE) return `${file.name} is ${formatSize(file.size)}; the limit is ${formatSize(MAX_FILE)} per file.`;
    if (existing.length >= MAX_FILES) return `A message can have at most ${MAX_FILES} attachments.`;
    const total = existing.reduce((n, a) => n + a.size, 0) + file.size;
    if (total > MAX_TOTAL) return `Attachments can be at most ${formatSize(MAX_TOTAL)} in total.`;
    return null;
  }

  // upload sends one file to a workspace and resolves with its attachment
  // record. onProgress gets 0..1. The returned object has abort(). xhrFactory
  // is replaceable for tests.
  function upload(workspace, file, name, onProgress, xhrFactory = () => new XMLHttpRequest()) {
    const xhr = xhrFactory();
    const promise = new Promise((resolve, reject) => {
      xhr.open("POST", "/workspaces/" + encodeURIComponent(workspace) + "/attachments");
      xhr.upload.onprogress = (e) => e.lengthComputable && onProgress(e.loaded / e.total);
      xhr.onerror = () => reject(new Error("Cannot reach the Mosso server. Is it still running?"));
      xhr.onabort = () => reject(Object.assign(new Error("Cancelled"), { aborted: true }));
      xhr.onload = () => {
        let data = null;
        try {
          data = JSON.parse(xhr.responseText);
        } catch (e) {
          /* not JSON */
        }
        if (xhr.status === 201 && data && data.attachments && data.attachments[0]) resolve(data.attachments[0]);
        else reject(new Error((data && data.error) || `Upload failed (HTTP ${xhr.status}).`));
      };
      const form = new FormData();
      form.append("files", file, name);
      xhr.send(form);
    });
    return { promise, abort: () => xhr.abort() };
  }

  return { MAX_FILE, MAX_TOTAL, MAX_FILES, formatSize, isImage, extension, nameFor, filesFromTransfer, checkFile, upload };
})();

if (typeof module !== "undefined") module.exports = Attach;
