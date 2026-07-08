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

Download the binary for your OS/arch and the `llm.txt` config from the
[latest release](https://github.com/deltatronix/dtx-agent/releases/latest), and
keep them **in the same folder** — the agent reads `llm.txt` from next to the
binary so you can pick your backend before the first run. Binaries are named
`dtx-agent-<os>-<arch>` (Windows: `.exe`), for `{windows,darwin,linux} × {amd64,arm64}`.

macOS / Linux one-liner (adjust `OS`/`ARCH`), into the current folder:

```sh
OS=$(uname -s | tr '[:upper:]' '[:lower:]'); ARCH=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
base=https://github.com/deltatronix/dtx-agent/releases/latest/download
curl -fsSL "$base/dtx-agent-${OS}-${ARCH}" -o dtx-agent && chmod +x dtx-agent
curl -fsSL "$base/llm.txt" -o llm.txt
```

To install on your `PATH`, move both together (e.g. `sudo mv dtx-agent llm.txt
/usr/local/bin/`), or skip `llm.txt` and edit the per-user copy the agent creates
on first run (see below).

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

The agent looks for `llm.txt` in this order:

1. **next to the binary** — the copy shipped in the release; edit it before you
   ever run the agent.
2. **`os.UserConfigDir()/dtx-agent/llm.txt`** — a per-user copy, written with the
   default on first run if step 1 found nothing (covers `go install`/source builds).

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
dtx-agent run     # or just `dtx-agent` once paired
```

It connects, advertises capabilities, and processes one job at a time,
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
ExecStart=/usr/local/bin/dtx-agent run
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
