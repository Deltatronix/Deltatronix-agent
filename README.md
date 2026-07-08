# dtx-agent — Deltatronix local compute agent

A single static Go binary that runs AI/compute jobs on **your** hardware for
Deltatronix. Phase 1 does local vision-LLM jobs via any OpenAI-compatible local
server ([Ollama](https://ollama.com) by default, or [LM Studio](https://lmstudio.ai),
llama.cpp, …): the platform sends a photo, a prompt, and a response schema; the
agent runs them against your local model and returns JSON. Prompts and feature
meaning live server-side — you never have to update the agent to get new AI features.

The agent is **outbound-only** (it dials `wss://api.deltatronix.io/compute/ws`),
so there is nothing to open on your firewall.

## Install

Download the binary for your OS/arch from the
[latest release](https://github.com/deltatronix/dtx-agent/releases/latest). On
first run the agent writes a per-user `llm.txt`; edit it via the **Settings**
window or directly. The `llm.txt` in the release is a reference copy. Desktop
binaries are named `dtx-agent-<os>-<arch>` (Windows: `.exe`) and ship for
`windows-amd64`, `darwin-{amd64,arm64}`, and `linux-amd64`. A CGO-free
`dtx-agent-linux-amd64-headless` binary (no GUI) is also published for
servers/containers — see [Docker](#docker).

macOS / Linux one-liner (adjust `OS`/`ARCH`), into the current folder:

```sh
OS=$(uname -s | tr '[:upper:]' '[:lower:]'); ARCH=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
base=https://github.com/deltatronix/dtx-agent/releases/latest/download
curl -fsSL "$base/dtx-agent-${OS}-${ARCH}" -o dtx-agent && chmod +x dtx-agent
```

To run it as a bare command, use the **Add to PATH** button in Settings or
`dtx-agent path add` (symlinks into `/usr/local/bin`, prompting for admin on
macOS/Linux; edits the per-user PATH on Windows). You don't need to keep
`llm.txt` alongside the binary — the agent creates and reads a per-user copy on
first run (see below).

## Set up Ollama

The agent does not bundle inference — Ollama owns the models and GPU drivers.

- **Windows:** `winget install Ollama.Ollama`
- **macOS:** `brew install ollama` (or download from <https://ollama.com/download>)
- **Linux:** `curl -fsSL https://ollama.com/install.sh | sh`

Then pull a vision model. The server decides which models are acceptable; if
Ollama is missing one, the agent prints the exact `ollama pull …` command on
start. A common choice:

```sh
ollama pull gemma3:4b
```

The agent advertises the `llm.vision` capability only while an acceptable vision
model is installed, and re-checks every 60s.

## Choosing the LLM backend

The agent speaks the **OpenAI-compatible API**, so it works with any local server
that exposes one — Ollama (default), [LM Studio](https://lmstudio.ai), llama.cpp,
vLLM, LocalAI, … It picks the backend from a plain-text `llm.txt`:

```
backend_url=http://localhost:11434/v1     # Ollama (default); LM Studio: :1234/v1
```

Set it in the **Settings** window (with one-click **Ollama** / **LM Studio**
buttons) — saving rewrites `llm.txt` and offers to restart. For LM Studio, start
its server from the **Developer** tab first. Headless users edit the key directly
and restart. Legacy files with a bare url line still work — the first uncommented
line wins if `backend_url=` is absent.

The authoritative `llm.txt` is the per-user copy at
**`os.UserConfigDir()/dtx-agent/llm.txt`**, written with the default on first run.
Backend changes take effect on the next restart.

### Logging options

`llm.txt` also accepts `key=value` lines for logging. Logging to a rotating file
is **on by default** so the GUI/tray build, which has no console, can show logs:

```
log=on                # on (default) | off
log_dir=              # folder for dtx-agent.log (default: the config folder)
log_max_size_mb=10    # rotate each file at this size
log_max_files=3       # rotated files to keep before overwriting
```

Set these in **Settings**, or edit directly for headless. Rotation is handled by
[lumberjack](https://github.com/natefinch/lumberjack). The legacy
`log_file=/full/path` key (single file, no rotation) is still honored.

## Pair

In the Deltatronix UI: **Settings → Local agents → Add agent** to get an 8-char
pairing code (valid 10 minutes). Then either paste it into the **Status** page of
the agent window, or from the CLI:

```sh
dtx-agent pair <code>
# custom backend:
dtx-agent pair <code> --api https://api.deltatronix.io
```

This stores `{ apiUrl, agentId, agentToken }` in
`os.UserConfigDir()/dtx-agent/config.json` (mode `0600`). **Disconnect** (Status
page) best-effort revokes the token server-side, then deletes the local config so
you can pair a different profile. Revoking from the platform UI also works.

## Run

```sh
dtx-agent run              # window + system-tray icon (default)
dtx-agent run --headless   # no GUI (servers/systemd/Docker)
dtx-agent                  # same as `run`
```

By default `run` opens a **window** with a left sidebar:

- **Status** — platform connection + local LLM-backend health, plus pairing (when
  unpaired) and Disconnect (when paired)
- **Settings** — backend URL, live-log console, log file/rotation options, Add to
  PATH, and Start-on-login
- **About** — version and what the agent does

Closing the window hides it to the **system tray** (**Current Status**, **Open**,
**Quit**); the app keeps running. When already paired it starts hidden in the tray;
when unpaired it opens the window so you can pair.

Use `--headless` for machines with no display; the GUI build also falls back to
headless automatically when it detects no display (Linux `DISPLAY`/`WAYLAND_DISPLAY`).
Either way it connects, advertises capabilities, and processes one job at a time,
reconnecting forever with jittered exponential backoff (1s→60s).

## Autostart

Toggle **Start on login** in Settings, or use the CLI (works in the headless build
too — handy on servers):

```sh
dtx-agent autostart enable                     # launch the GUI app on login
dtx-agent autostart enable --headless          # launch `run --headless` (servers)
dtx-agent autostart enable --mode systemd      # Linux: systemd --user instead of XDG
dtx-agent autostart disable
```

Under the hood: macOS a LaunchAgent plist (`~/Library/LaunchAgents/io.deltatronix.agent.plist`),
Windows an HKCU `…\Run` value, Linux either an XDG `~/.config/autostart/dtx-agent.desktop`
(default) or a `~/.config/systemd/user/dtx-agent.service`. All are per-user (no admin).

## Docker

The headless binary is CGO-free and runs on a minimal base — see [`Dockerfile`](Dockerfile).
Pair once on any machine, then mount the resulting `config.json` into the container:

```sh
docker build -t dtx-agent .
docker run --rm \
  -v "$PWD/config.json:/home/nonroot/.config/dtx-agent/config.json:ro" \
  dtx-agent
```

## Contract notes

- WebSocket frames follow `Deltatronix-backend/docs/plans/compute-agents-phase-1.md` §1.
- The `job.assign` frame is specified with both an envelope `type` (`"job.assign"`)
  and a job `type` field — a duplicate JSON key. The agent routes on frame kind
  and falls back to payload-presence, so it handles the assignment whichever way
  the backend serializes that key.
