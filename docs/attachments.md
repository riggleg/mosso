# Attachments

Attach screenshots, images and files to a chat message in a workspace. Claude
can look at them: images are seen visually, PDFs and other files are read with
Claude Code's own tools.

## Use it

- **Paste**: copy a screenshot (`Cmd+Ctrl+Shift+4` copies a region to the
  clipboard), or an image from a browser, Preview and so on, click the
  composer and press `Cmd+V`. It appears as a thumbnail above the input. Text
  paste works as before.
- **Drag and drop** files onto the workspace.
- **Paperclip** button: choose one or more files.

All three end in the same code. Each file uploads as soon as it is added, with
a progress bar; you can remove it, or retry it if it failed. Send is disabled
until every upload has finished. A message may be attachments only. Click a
thumbnail to see it larger. In the history, images show as thumbnails and
other files download from the workspace's sandbox. A refresh keeps them, as
long as the sandbox lives; afterwards a thumbnail turns into a notice that the
attachment is no longer available.

Limits (checked in the browser for quick feedback, and again on the server):
25 MiB per file, 10 files and 40 MiB per message, 256 MiB per workspace. Names
are reduced to a safe base name. Any file type is accepted; only PNG, JPEG,
WebP and GIF (decided by content, not by name) are shown inline. SVG, HTML and
everything else is served only as a download.

## Clipboard limitations

The page uses the browser's `paste` event and `clipboardData`; no extension or
native app is involved. What the browser puts in `clipboardData` is up to the
browser:

| Clipboard content | Behavior |
| ----------------- | -------- |
| Screenshot or image from any app (PNG/TIFF) | Attached. Tested in Chromium; Safari and Chrome on macOS expose these as a file in `clipboardData`. |
| "Copy Image" in a browser | The image is attached, not its URL or HTML, even though the clipboard also holds both. |
| Files copied in Finder (`Cmd+C`) | Chrome and Safari expose the copied files in `clipboardData.files` for many types, but this depends on the browser version and file type. If the browser exposes none, Mosso says so; drag the file in or use the paperclip. **Not verified on a Mac by the author of this change.** |
| Folders, or files the browser hides | Not accessible to a web page. Same message. |
| Images dragged from another web page | Often arrive as a URL, which Mosso's security policy (`connect-src 'self'`) does not let the page download. Mosso suggests Copy Image then `Cmd+V`. |

A pasted image is named `Pasted image <date>.png`.

## How it works

1. `POST /workspaces/{name}/attachments` takes `multipart/form-data` with
   `files` parts (the UI sends one request per file, for progress and retry).
   The server reads it in memory, validates it, writes it into the sandbox with
   Vercel's documented write-files API (gzipped tar plus the `x-cwd` header),
   and returns `{"attachments":[{id,name,size,type,image}]}`. Nothing is
   written on the Mosso machine and nothing about the file goes to the log.
2. Files live at `/tmp/mosso-attachments/<id>/<name>` in the workspace's
   sandbox: outside the repository, mode 0644 (never executable), and a random
   `<id>` per file so equal names never collide. Mosso never runs, commits or
   adds them to Git. They are deleted with the sandbox when the workspace is
   terminated or expires, and with it Mosso's in-memory records.
3. `POST /workspaces/{name}/chat` accepts `"attachments": ["<id>", …]` next to
   `prompt` (which may then be empty). The ids must belong to that workspace.
   The user's entry in the chat log carries the attachments, so every browser
   tab and reload shows them. Previously uploaded ids can be attached again,
   and Claude's session remembers earlier paths for follow-ups.
4. Claude's prompt gets a note listing each file's sandbox path and type, and
   telling it to open them with the Read tool, which shows images to the model
   visually (and reads PDFs). File contents are never put into the prompt.
   Credentials are not involved in any attachment step.
5. `GET /workspaces/{name}/attachments/{id}` reads the file back from the
   sandbox with the read-file API: images inline, with `?download=1` or for any
   other type as `application/octet-stream` attachments, all with `nosniff`
   and `Content-Security-Policy: default-src 'none'; sandbox`. `410` means the
   sandbox is gone.

The upload endpoint accepts `multipart/form-data` only on that route; the
loopback `Host` and `Origin` checks apply to it like to every endpoint.

## Tests

- `go test ./internal/server` covers uploads, safe names and collisions, path
  traversal, limits, retries, isolation between workspaces, safe serving,
  Claude receiving the paths, history replay and cleanup with a fake sandbox.
- `node --test internal/server/webtest/attachments.test.js` covers the paste,
  drop, limit and upload logic of `web/assets/attachments.js`.
- The UI was also driven in headless Chromium against the real Go server over
  a fake sandbox (`MOSSO_FAKE_ADDR=127.0.0.1:8099 go test ./internal/server
  -run TestServeFakeForBrowser -timeout 1h`): synthetic `paste` events with an
  image plus text, drop, the file picker, removal, preview, sending with and
  without text, and a refresh. These use real `DataTransfer` objects but not
  the macOS clipboard itself.
- `MOSSO_E2E=1 go test ./internal/server -run TestAttachmentVisionEndToEnd -v
  -timeout 20m` uploads an orange image to a real sandbox and asks real
  Claude for its color. It needs Vercel credentials and a Claude token and
  uses a little subscription; it is opt-in.
