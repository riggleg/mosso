# Sandbox environments: setup scripts and custom images

By default, every workspace's sandbox runs Vercel's `node22` runtime
(Amazon Linux 2023, with `dnf` and passwordless `sudo`), and Mosso installs
only Git, Claude Code and its worker. Two settings customize the
environment of new workspaces:

- A **setup script**: Bash that runs once in each new workspace, after the
  repository is cloned and before Claude starts. Use it to install system
  packages, [mise](https://mise.jdx.dev), languages and other tools.
- A **custom image**: a container image from
  [Vercel Container Registry](https://vercel.com/docs/container-registry)
  that the sandbox boots from instead of `node22`, with your tools already
  installed. Startup is faster and the same for every workspace.

Both have a default, set in the web UI and saved in Mosso's database, and
both can be changed for one workspace when you create it. Leave both empty
and workspaces are created exactly as before.

## Use it

- **Defaults.** In the sidebar footer, click **Change** next to *Sandbox*
  (or *Model*). The *Settings for new workspaces* dialog has the default
  model, sandbox image and setup script. Changing them never affects
  existing workspaces. A [section](sections.md) can have its own image and
  setup script instead, used for workspaces created in it.
- **One workspace.** The **+ New** dialog has a collapsed *Sandbox
  environment* section, prefilled with the defaults. Edit the image or the
  script for this workspace only, or clear them for none.
- **While it is set up.** The workspace opens while it is created. The bar
  above the composer says *Running the setup script…*, and the script's
  output streams into the chat under *Setup script output*. Your first
  message waits for it.
- **Afterwards.** The header shows the image (or `node22 (built-in)`) and
  `· setup` next to the model; hover over it to see the setup script. The
  setup output stays in the chat.
- **If it fails.** If the script exits with an error, the workspace is not
  created. Mosso destroys the sandbox and shows the error with the end of
  the script's output: in the New workspace dialog if it is still open, in a
  banner otherwise. Your first message is kept in the dialog.

## Setup scripts

Mosso writes the script to `/vercel/sandbox/.mosso/setup.sh` in the sandbox
and runs it:

- **once per workspace**, when it is created: after the clone, before
  Claude Code is installed, the worker starts or the first message runs. A
  persistent sandbox keeps what the script installed in its snapshot, so
  resuming a stopped sandbox or restarting Mosso does not run it again. To
  run it again by hand, ask Claude to run `bash /vercel/sandbox/.mosso/setup.sh`.
- **in the repository checkout** (for example `/vercel/sandbox/widgets`), so
  it can read the repository's `mise.toml`, `.tool-versions`, `package.json`
  and so on.
- **with `bash -e -o pipefail`**: it stops at the first command that fails,
  including inside a pipe. Its stdin is empty, so it cannot prompt; use
  `-y` flags. stderr and stdout are combined.
- **as the sandbox's default user**, with `sudo` available on the built-in
  runtime, and with `DEBIAN_FRONTEND=noninteractive` and `MISE_YES=1` set
  for unattended installs.
- **without credentials.** The script never gets your Claude or GitHub
  tokens: they are not in its environment, and the sandbox does not hold
  them. A private repository's token is only given to the clone command,
  which has finished by then. Do not put secrets in the script itself; it
  is saved in Mosso's database and copied into the sandbox.
- **for up to 20 minutes.** Longer, and it is stopped and the workspace is
  not created. It also counts against the sandbox session (45 minutes by
  default; see [idle-shutdown.md](idle-shutdown.md)).

Its full output is also kept in `/vercel/sandbox/.mosso/setup.log`. The chat
shows the first 256 KiB. Scripts can be up to 64 KiB.

### Making tools available to Claude

Claude Code, its Bash tool and the worker run non-interactively, so they do
not read `~/.bashrc`, and `mise activate` (a prompt hook) does nothing for
them. Instead, Mosso keeps the workspace's environment in a file,
`/vercel/sandbox/.mosso/env`, which the worker loads every time it starts,
including after a sandbox resumes. Claude Code and every command it runs
inherit it. The file starts as:

```bash
export PATH="$HOME/.local/bin:$HOME/.local/share/mise/shims:$PATH"
```

so tools in `~/.local/bin` (mise, uv, pipx, Claude Code) and mise's
**shims** are found without further setup. To add anything else, append
`export` lines to the file, whose path is in `$MOSSO_ENV`:

```bash
echo 'export PATH="$HOME/.cargo/bin:$PATH"' >> "$MOSSO_ENV"
echo 'export GOFLAGS=-mod=mod' >> "$MOSSO_ENV"
```

The script itself runs with this file already loaded, so a tool it installs
into `~/.local/bin` can be used on the next line. Mosso also sources the file
from `~/.bashrc` for interactive shells (for example
`vercel sandbox connect`).

### Examples

mise, with the versions the repository pins in `mise.toml` or
`.tool-versions`:

```bash
curl https://mise.run | sh
mise trust            # mise refuses untrusted config files
mise install
```

mise with explicit versions, for repositories that do not pin any:

```bash
curl https://mise.run | sh
mise use -g node@24 python@3.14 go@1.27
```

System packages on the built-in runtime (Amazon Linux 2023):

```bash
sudo dnf install -y jq postgresql16
```

System packages on Ubuntu-based images such as `vercel/sandbox/universal`:

```bash
sudo apt-get update && sudo apt-get install -y jq
```

Project dependencies, so Claude can run the tests right away:

```bash
curl https://mise.run | sh
mise trust && mise install
npm ci
```

## Custom images

Set the image to a Vercel Container Registry reference. Mosso creates the
sandbox from it with Vercel's image API (`POST /v3/sandboxes` with
`image`, as the official SDK does). Without an image, it keeps creating
`node22` sandboxes (`POST /v2/sandboxes` with `runtime`). References look
like this:

| Reference | Resolves to |
| --- | --- |
| `my-image` | the `latest` tag in Mosso's Vercel project (`VERCEL_PROJECT_ID`) |
| `my-image:v1`, `my-image@sha256:…` | a tag or digest in Mosso's project |
| `team-slug/project-slug/my-image:v1` | an image in another project or team, shared with yours or public |
| `vcr.vercel.com/team-slug/project-slug/my-image:v1` | the same, with the registry host |
| `vercel/sandbox/universal`, `vercel/sandbox/node:24`, `vercel/sandbox/python:3.14`, `vercel/sandbox/ubuntu` | Vercel's [managed images](https://vercel.com/docs/sandbox/concepts/images#vercel-managed-images) |

Pin a digest (`@sha256:…`) if every workspace must get exactly the same
image. The image each workspace was created from is saved with it.

Mosso does not build or publish images. Build and push one yourself:

```bash
vercel vcr login docker
vercel vcr build docker . my-image:v1 --push   # builds linux/amd64 and pushes to Mosso's project
```

Then set the image to `my-image:v1`. See Vercel's
[Images](https://vercel.com/docs/sandbox/concepts/images) and
[Container Registry](https://vercel.com/docs/container-registry) docs.

What the image needs, and what to expect:

- **`linux/amd64`**, and *Ready* in Vercel Container Registry. A freshly
  pushed image is prepared for Sandbox first. Until then, creating a
  workspace fails with Vercel's `image_not_ready` error; try again in a
  moment.
- **`bash`, `curl` and `git`**. Mosso clones with `git`, installs Claude Code
  with `curl` and talks to its worker with `curl`. (On images with `dnf`,
  Mosso installs a missing `git` itself.) Vercel's managed images have all
  three, except the bare `ubuntu` one.
- A writable **`/vercel/sandbox`** for the checkout and Mosso's state, as in
  Vercel's own images.
- Vercel does **not** run the image's `ENTRYPOINT` or `CMD`. Mosso runs its
  own commands; the setup script is the place for anything that must run
  first.
- Tools must be on the default `PATH` (for example in `/usr/local/bin`), or
  added to it with the setup script and `$MOSSO_ENV`.

An image and a setup script work together: bake the slow, stable parts
(system packages, language runtimes) into the image, and keep the
repository-specific steps (`mise install`, `npm ci`) in the script.

## Configuration and storage

The defaults are saved in the `settings` table under `default_image` and
`default_setup_script`. Each workspace's image and setup script are saved in
the `image` and `setup_script` columns of the `workspaces` table (schema
migration 3). Workspaces from before have neither, and ran `node22` with no
setup. See [history.md](history.md).

### API

| Method & path | Description |
| --- | --- |
| `GET /settings` | Also returns `default_image`, `default_setup_script` (both `""` when unset) and `built_in_runtime` (`"node22"`). |
| `POST /settings` | Accepts any of `default_model`, `default_image` and `default_setup_script`; omitted ones are unchanged, `""` clears the image or script. All are saved together or not at all. `400` for an invalid image reference, a script over 64 KiB or one with NUL bytes, or an empty body. |
| `POST /workspaces` | Accepts `"image"` and `"setup_script"`. Omitted, the workspace gets the defaults; `""` asks for the built-in runtime or no script. `400` if invalid, checked before a sandbox is created. `422` if the setup script failed, `502` if it could not run (the error includes the end of its output); the sandbox is destroyed either way. |
| `GET /workspaces`, `GET /workspaces/{name}` | Each workspace has `image` and `setup_script` when set, and `step` (`cloning`, `setup` or `installing`) while its status is `creating`. |

Chat entries of type `setup` carry the setup script's output, in order.
