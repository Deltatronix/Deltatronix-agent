# Deltatronix Agent

Run Deltatronix AI vision tasks on your own computer. The agent connects your
Deltatronix account to a local model server, processes assigned tasks, and sends
the results back to Deltatronix.

Use the desktop app for a window and system-tray controls, or the headless version
for a terminal or server. The agent initiates its connection to Deltatronix, so
you do not need to set up port forwarding.

## What you need

- **Git and Go 1.26 or newer** to build the agent.
- **A local model server**, such as [Ollama](https://ollama.com/download) or
  [LM Studio](https://lmstudio.ai), with a compatible vision model installed.
- **A Deltatronix account and pairing code** to connect the agent and receive tasks.

You can build this repository on its own. Pairing and running Deltatronix tasks
require access to the Deltatronix service. The agent does not include a model
server or download models for you.

## Build from source

Clone this repository, or fork it first and use your fork's URL:

```sh
git clone https://github.com/Deltatronix/Deltatronix-agent.git
cd Deltatronix-agent
```

Choose one of the following builds. Go downloads the required packages during
the first build.

### Desktop app

The desktop app uses Fyne and requires a C compiler and platform development
libraries. Follow the [Fyne setup guide](https://docs.fyne.io/started/) for your
operating system. On macOS, install the Xcode Command Line Tools; on Windows,
use a compatible C compiler as described in that guide.

On Ubuntu or Debian, install the desktop dependencies:

```sh
sudo apt-get update
sudo apt-get install -y build-essential libgtk-3-dev libayatana-appindicator3-dev libgl1-mesa-dev xorg-dev libxkbcommon-dev
```

Build on macOS or Linux:

```sh
CGO_ENABLED=1 go build -tags gui -o dtx-agent .
./dtx-agent
```

Build on Windows using PowerShell:

```powershell
$env:CGO_ENABLED = "1"
go build -tags gui -ldflags "-H=windowsgui" -o dtx-agent.exe .
.\dtx-agent.exe
```

### Headless agent

This build has no window or tray icon and does not require the desktop libraries:

```sh
go build -o dtx-agent .
```

On Windows, use `go build -o dtx-agent.exe .` and run commands with
`.\dtx-agent.exe` instead of `./dtx-agent`.

Pair the agent before starting it, as described below. Always use
`run --headless` with this build.

## Connect your agent

### 1. Start your local model server

Install and start Ollama or LM Studio. In LM Studio, load a vision model and
start its local server from the **Developer** tab.

The default connection is Ollama at `http://localhost:11434/v1`. For LM Studio,
open the agent's **Settings**, select **LM Studio**, save, and restart. Its
default URL is `http://localhost:1234/v1`.

For a headless setup, edit the per-user `llm.txt` file described under
[Configuration](#configuration). Running `./dtx-agent run --headless` once creates
that file, even if the agent has not been paired yet.

### 2. Pair with Deltatronix

In Deltatronix, open **Settings → Local agents → Add agent** and get a pairing
code. Enter it on the desktop app's **Status** page and select **Pair**.

From a terminal, replace `YOUR_PAIRING_CODE` with your code:

```sh
./dtx-agent pair YOUR_PAIRING_CODE
./dtx-agent run --headless
```

### 3. Check that the agent is ready

The **Status** page shows the Deltatronix connection and local model server
status. The agent checks for a compatible vision model and reports missing
models in its logs. Install a model requested by the agent; with Ollama, use
`ollama pull` followed by the model name shown in the log.

Keep the model server and agent running while you use local AI features in
Deltatronix. The agent retries automatically if a connection is interrupted.

## Everyday use

- **Open the desktop app:** run `./dtx-agent`.
- **Find a running app:** use its system-tray icon and select **Open**. A paired
  agent starts in the tray; closing its window keeps it running.
- **Stop the desktop app:** select **Quit** from the tray menu.
- **Run without a window:** use `./dtx-agent run --headless`; press Ctrl+C to stop
  an agent running in your terminal.
- **Disconnect your account:** select **Disconnect** on the desktop **Status**
  page, or revoke the agent from Deltatronix.
- **Start on login:** enable **Start automatically on login** in Settings.

The startup and PATH options are also available from a terminal:

| Action | Command |
| --- | --- |
| Start the desktop app on login | `./dtx-agent autostart enable` |
| Start the headless agent on login | `./dtx-agent autostart enable --headless` |
| Turn off startup on login | `./dtx-agent autostart disable` |
| Add the agent to PATH | `./dtx-agent path add` |
| Remove it from PATH | `./dtx-agent path remove` |

Choose the startup command that matches your build. On Linux, add
`--mode systemd` to `autostart enable --headless` to use a user systemd service.
After adding the agent to PATH, you can use `dtx-agent` without the `./` prefix.

## Configuration

The agent stores its settings in your user configuration folder:

| Operating system | Default folder |
| --- | --- |
| macOS | `~/Library/Application Support/dtx-agent/` |
| Linux | `~/.config/dtx-agent/` (or `$XDG_CONFIG_HOME/dtx-agent/` if set) |
| Windows | `%AppData%\dtx-agent\` |

- **`llm.txt`** contains the model server URL and logging settings. The agent
  creates it on first run. Edit it through **Settings**, or directly for a
  headless setup, then restart the agent.
- **`config.json`** stores your pairing credentials. Keep it private and out of
  your fork or commits.
- **`dtx-agent.log`** contains runtime logs by default. Desktop users can also
  view logs in **Settings**.

Example `llm.txt`:

```ini
backend_url=http://localhost:11434/v1
log=on
log_dir=
log_max_size_mb=10
log_max_files=3
```

Leave `log_dir` empty to keep logs in the configuration folder. The
[`llm.txt`](llm.txt) in this repository is the default template; edit your
per-user copy to change the running agent's settings.

## Troubleshooting

| Problem | What to check |
| --- | --- |
| “This build has no GUI” | Run with `run --headless`, or rebuild with `-tags gui` for the desktop app. |
| “Not paired” | Pair from the desktop Status page or run `./dtx-agent pair YOUR_PAIRING_CODE`. |
| Model server is unreachable | Start your model server and check `backend_url` in Settings or `llm.txt`. |
| Connected, but no vision model is available | Check the logs for the requested model and install or load it in your model server. |
| The desktop window is missing | Check the system tray and select **Open**. |
| Desktop build fails on graphics or C libraries | Check the Fyne prerequisites and the desktop dependencies listed above. |

## Development

Run the tests with:

```sh
go test ./...
```

The [`Dockerfile`](Dockerfile) builds a headless container. Mount your paired
`config.json` and your `llm.txt` in `/home/nonroot/.config/dtx-agent/`. Set
`backend_url` to a model server address the container can reach; inside a
container, `localhost` refers to the container itself.

Issues and pull requests are disabled for this repository. Developers can fork,
clone, and build the agent, and keep their changes in their own forks.
