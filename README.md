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
first run the agent writes a per-user `llm.txt` you can edit (directly, or via the
tray's **Parameters File** item). The `llm.txt` in the release is a reference copy.
Binaries are named `dtx-agent-<os>-<arch>` (Windows: `.exe`); the tray build ships
for `windows-amd64`, `darwin-{amd64,arm64}`, and `linux-amd64`.

macOS / Linux one-liner (adjust `OS`/`ARCH`), into the current folder:

```sh
OS=$(uname -s | tr '[:upper:]' '[:lower:]'); ARCH=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
base=https://github.com/deltatronix/dtx-agent/releases/latest/download
curl -fsSL "$base/dtx-agent-${OS}-${ARCH}" -o dtx-agent && chmod +x dtx-agent
```

To install on your `PATH`, move the binary (e.g. `sudo mv dtx-agent
/usr/local/bin/`). You don't need to keep `llm.txt` alongside it — the agent
creates and reads a per-user copy on first run (see below).

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
vLLM, LocalAI, … It picks the backend from a plain-text `llm.txt` that ships with
the release, Ollama active and LM Studio commented out:

```
# Ollama (default)
http://localhost:11434/v1

# LM Studio
# http://localhost:1234/v1
```

To switch, uncomment exactly one url line (comment the other) and restart the
agent — the first uncommented line wins. For LM Studio, start its server from the
**Developer** tab first.

The authoritative `llm.txt` is the per-user copy at
**`os.UserConfigDir()/dtx-agent/llm.txt`**, written with the default on first run.
Open it from the tray (**Parameters File**) or edit it directly. Backend changes
take effect on the next restart.

### Logging options

`llm.txt` also accepts optional `key=value` lines for logging (a bare url line
stays the backend). Logging to a file is **on by default** so the tray build,
which has no console, can show logs:

```
log=on                                  # on (default) | off
log_file=/full/path/to/dtx-agent.log    # default: <config dir>/dtx-agent.log
```

## Pair

In the Deltatronix UI: **Settings → Local agents → Add agent** to get an 8-char
pairing code (valid 10 minutes). Then:

```sh
dtx-agent pair <code>
# custom backend:
dtx-agent pair <code> --api https://api.deltatronix.io
```

This stores `{ apiUrl, agentId, agentToken }` in
`os.UserConfigDir()/dtx-agent/config.json` (mode `0600`). Revoke an agent from
the same UI (it hard-deletes the token).

## Run

```sh
dtx-agent run              # system-tray icon + menu (default)
dtx-agent run --headless   # no GUI (servers/systemd)
dtx-agent                  # runs if already paired
```

By default `run` shows a **system-tray icon** with a native menu:

- a disabled **status** line — `Connecting…` / `Connected` / `Reconnecting…` / `No LLM backend`
- **Parameters File** — opens `llm.txt` in your editor
- **Open Console** — a terminal streaming the log live (`tail -f`)
- **Open Logs** — opens the log file
- **Quit**

Use `--headless` for machines with no display; the tray build also falls back to
headless automatically when it detects no display (Linux `DISPLAY`/`WAYLAND_DISPLAY`).

Either way it connects, advertises capabilities, and processes one job at a time,
reconnecting forever with jittered exponential backoff (1s→60s).

## Autostart

Docs only — no service installer ships yet.

### macOS (launchd)

`~/Library/LaunchAgents/io.deltatronix.agent.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>io.deltatronix.agent</string>
  <key>ProgramArguments</key>
  <array><string>/usr/local/bin/dtx-agent</string><string>run</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
</dict>
</plist>
```

```sh
launchctl load ~/Library/LaunchAgents/io.deltatronix.agent.plist
```

### Linux (systemd user unit)

`~/.config/systemd/user/dtx-agent.service`:

```ini
[Unit]
Description=Deltatronix compute agent
After=network-online.target

[Service]
ExecStart=/usr/local/bin/dtx-agent run --headless
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
```

```sh
systemctl --user daemon-reload
systemctl --user enable --now dtx-agent
loginctl enable-linger "$USER"   # keep running after logout
```

### Windows (Task Scheduler)

```powershell
$exe = "C:\Program Files\dtx-agent\dtx-agent.exe"
$action  = New-ScheduledTaskAction -Execute $exe -Argument "run"
$trigger = New-ScheduledTaskTrigger -AtLogOn
Register-ScheduledTask -TaskName "dtx-agent" -Action $action -Trigger $trigger -RunLevel Limited
```

## Contract notes

- WebSocket frames follow `Deltatronix-backend/docs/plans/compute-agents-phase-1.md` §1.
- The `job.assign` frame is specified with both an envelope `type` (`"job.assign"`)
  and a job `type` field — a duplicate JSON key. The agent routes on frame kind
  and falls back to payload-presence, so it handles the assignment whichever way
  the backend serializes that key.
