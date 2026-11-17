# Images in Claude's activity

Screenshots, charts and other images Claude produces show directly in the
chat, next to the tool activity that made them. You don't need to download
anything from the sandbox, and Claude doesn't need to upload anything.

## What is shown

| Source | Example | Where it shows |
| --- | --- | --- |
| A tool returns an image | Claude Code's Read tool on a PNG; a Playwright MCP `browser_take_screenshot` | Under that tool call |
| A tool writes an image file in the working directory, or directly in `/tmp` | `npx playwright screenshot … screenshots/home.png`, `python chart.py` writing `output/chart.webp`, a `design/mockup.svg` | Under that tool call, captioned with the file's path |
| Claude's reply refers to a local image in Markdown | `![Home page](screenshots/home.png)` | Inline in the reply |
| An image block outside a tool result | (rare) | On its own |

Click a thumbnail for a larger preview. **Download** is in the preview and
next to each caption. Images stay after a refresh, after Mosso restarts,
and after the sandbox stops and resumes (see below). Claude still knows the
paths of the files it wrote, so a follow-up such as "make the header in that
screenshot bigger" works as usual.

**Supported types:** PNG, JPEG, GIF, WebP and SVG, decided by the content,
not by the file name. A `.png` that holds text is not shown. Each image can
be at most 10 MiB.

## How it works

1. **Claude Code's output.** In `stream-json`, Claude Code (checked against
   the bundle of version 2.1.311) sends an image a tool returned as a
   `tool_result` content block
   `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"…"}}`.
   This shape is used for the Read tool and for MCP tools (whose
   `{type:"image",mimeType,data}` results it converts). The same data is
   repeated in the message's `tool_use_result`, which Mosso ignores.
   `internal/claude` decodes the block, sniffs its type and keeps the bytes
   in memory only (`Image.Data` never goes to JSON). URL sources are not
   fetched. Malformed, unsupported or oversized images leave an `[image]`
   placeholder in the tool's text, as before. Nothing is made up from a
   textual description.
2. **The worker stores images by content.** Before an event is written to
   the event log, the worker (`internal/worker/images.go`) writes each image
   to `/vercel/sandbox/.mosso/images/<id>`. The `<id>` is the first 32 hex
   digits of the content's SHA-256. The event keeps only
   `{"id","type","size","name","path","ref"}`, so the event log and Mosso's
   database never hold image bytes. The store is outside the repository, so
   nothing is added to Git. Because it is a copy, the image stays as Claude
   showed it even if the file later changes or is deleted.
3. **New image files.** After each tool result, the worker looks for image
   files created or changed since its last look (and since the task
   started). It looks in the working directory, skipping hidden directories
   (`.git`, `.next`, `.playwright-mcp`, …), `node_modules`, `vendor`,
   `target`, `venv` and `__pycache__`, and directly in `/tmp`. Each new
   version of a file is shown once. Content already shown in the same task
   is not shown again.
4. **Markdown references.** In Claude's text, `![alt](path)`,
   `![alt](<path with spaces>)` and `file:///abs/path` references are
   resolved by the worker against the working directory, copied into the
   store and recorded with their `ref`. The web UI matches the reference to
   that record. It never loads the reference itself.
5. **Serving.** `GET /workspaces/{name}/images/{id}` serves an image only if
   that workspace's chat shows that ID. It reads `.mosso/images/<id>` with
   Vercel's read-file API, and refuses the file (404) unless its SHA-256
   matches the ID and its content is a supported image.
6. **Local cache.** When an image of at most 2 MiB arrives, Mosso fetches
   it in the background while the sandbox is running. It saves it in the
   data directory, under `images/<workspace>/<id>` (next to `mosso.db`,
   directory mode 0700), up to 64 MiB per workspace. Cached images are
   checked against their ID and served without the sandbox. Cached copies are
   kept with the workspace's history. Delete the directory to drop them.

## Stopped and resumed sandboxes

- **Running:** every image is served (from the cache or the sandbox).
- **Stopped (session ended), or Mosso restarted meanwhile:** cached images
  still show. Larger ones show *"Not available while the sandbox is stopped
  or gone; resume it and reload"*. Mosso never resumes a sandbox just to
  show a thumbnail. **Resume sandbox**, or your next message, brings them
  back: the image store is in the snapshotted filesystem.
- **Terminated or gone:** cached images stay visible in the history;
  others are unavailable.
- **Deleted or changed in the sandbox:** *"The image is no longer in the
  sandbox"*. A file the browser cannot decode says *"Cannot be displayed;
  download it instead"*.

## Security

All of this content is treated as untrusted (it may come from a
prompt-injected Claude):

- **No arbitrary reads.** The server reads only `.mosso/images/<id>` (a
  validated 32-hex ID that the workspace's own chat shows) from that
  workspace's own sandbox. Other workspaces' IDs are 404.
- **Confinement in the sandbox.** The worker copies a file only if it
  resolves, symlinks included, to a regular file under `/vercel/sandbox` or
  `/tmp`, and outside the worker's own state (except `attachments/`). The
  final open uses `O_NOFOLLOW`. FIFOs, devices, files over 10 MiB, `..`
  escapes and symlinks pointing elsewhere are refused.
- **Content checks.** Content must sniff as PNG, JPEG, GIF or WebP, or parse
  as XML with an `svg` root element. On the server it must also match its
  SHA-256 ID, so a file Claude swaps into the store (for example a symlink
  to a secret) is refused.
- **Safe responses.** Every response has `X-Content-Type-Options: nosniff`,
  `Cross-Origin-Resource-Policy: same-origin` and
  `Content-Security-Policy: default-src 'none'; img-src data:; style-src 'unsafe-inline'; sandbox`.
  SVG is served as `image/svg+xml`. In the chat it is shown with `<img>`,
  where scripts never run. Opened directly, the CSP `sandbox` (without
  `allow-scripts`) blocks its scripts, which was verified in Chromium with
  an SVG carrying `onload` and `<script>`. Downloads are
  `application/octet-stream` attachments.
- **Markdown.** The renderer never sets an image source from Claude's text.
  Only images the worker resolved are shown, through Mosso's URLs. Remote
  `http(s)` images become links. `file:`, `data:`, `javascript:` and
  unresolved paths show as `[image: alt]`. The web UI's Content Security
  Policy is unchanged (`img-src 'self' data: blob:`).
- **Limits.** 10 MiB per image, 40 images per task, 10 per event, 20,000
  entries visited per scan, a 2 MiB / 64 MiB local cache. The loopback-only
  `Host`/`Origin` checks apply to the image endpoint like to every other.
  No credentials are involved.

## Limitations

- **Real-CLI verification.** The `stream-json` image shape was confirmed in
  the Claude Code 2.1.311 bundle, but not from a live run, because no
  logged-in Claude Code was available when this was built. The parser
  tests use that shape. To check it against your account, run
  `MOSSO_E2E=1 go test ./internal/server -run TestImageResultsEndToEnd -v
  -timeout 20m`, which needs Vercel credentials and a Claude token. Real
  Claude Code in a real sandbox reads a PNG, and the test expects the tool
  result to carry it as an image.
- An image file written outside the working directory and `/tmp` (for
  example in `~`) is not picked up, and a Markdown reference to it shows as
  `[image: …]`.
- Images written by a tool that runs in the background (for example a dev
  server writing files later) show under the next tool result.
- A build that writes many images outside the skipped directories can add
  up to 10 thumbnails per tool call (40 per task).
- Very large `stream-json` lines (over 16 MiB, about an 8 MiB image, because
  Claude Code repeats the data) are skipped by the parser, as before.
- Workers started by an earlier Mosso (protocol 2) are replaced when idle.
  Until then they record no images.

## Tests

- `go test ./internal/claude`: image blocks in tool results (the Read and
  MCP shapes), top-level image blocks, rejected sources (URL, bad base64,
  HTML posing as PNG, too large), and no image bytes in JSON.
- `go test ./internal/worker`: type sniffing (including SVG versus HTML),
  confinement (symlink escapes, symlinked directories, `..`, the state
  directory, FIFOs, size), Markdown references (relative, `<…>`, `file://`,
  remote, `data:`, traversal, missing), the new-file scan (skipped
  directories, `/tmp`, repeats, new versions), bounds, and a fake `claude`
  printing the real `stream-json` shape. That test checks that the event log
  has no image data and that a restarted worker replays the references.
- `go test ./internal/server` (mocked Vercel API, real in-process worker):
  serving and headers (PNG, sandboxed SVG, downloads, names), unknown and
  invalid IDs, workspace isolation, the local cache (small cached, large
  not, 0700), a Mosso restart with the sandbox stopped (cached images shown,
  others 410, sandbox not resumed), resume, a tampered and a deleted image
  in the sandbox, and termination.
- `node --test internal/server/webtest/markdown.test.js`: Markdown image
  syntax, only caller-provided nodes, no loads of `javascript:`, `data:`,
  `file:` or remote references, and no markup injection.
- Browser: `MOSSO_FAKE_ADDR=127.0.0.1:8099 MOSSO_FAKE_SCENARIO=images go test
  ./internal/server -run TestServeFakeForBrowser -timeout 1h` (add
  `MOSSO_FAKE_STOP=1` for a stopped sandbox). This was driven in headless
  Chromium with Playwright: thumbnails, the inline Markdown image, the
  missing and remote references, the preview, both downloads, a refresh,
  the stopped state, and an SVG opened directly without its scripts
  running.

Not run by the author of this change: `TestImageResultsEndToEnd` and the
other opt-in live tests (see [worker.md](worker.md)) with a real Vercel
Sandbox and Claude Code, because no credentials were available. To
check the acceptance flow by hand, create a workspace and ask Claude to take
a screenshot of a web page with Playwright (for example
`npx -y playwright@1 screenshot https://example.com shot.png`, after `npx
playwright install --with-deps chromium`). Then check that the screenshot
shows, opens and downloads, and that it survives a refresh, a Mosso
restart, and **Stop**/resume of the sandbox.
