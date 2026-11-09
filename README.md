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

## Local tool chat: companion-side support

Tool chat is opt-in. In your per-user `llm.txt`, set `chat_model` to the **exact**
installed ID from the model server's `/v1/models` response, and set
`chat_context_tokens` to a positive cap you have verified for that model. For
example, an installed Ollama `gemma4:latest` can be selected with:

```ini
backend_url=http://127.0.0.1:11434/v1
chat_model=gemma4:latest
chat_context_tokens=131072
```

Restart after editing. The GUI preserves these two keys when saving settings;
edit them in the file. Empty model and zero cap disable chat. Missing, invalid,
or uninstalled selections never pick another model. Chat accepts only loopback
HTTP(S) `/v1` servers, disables HTTP proxies and rejects redirects. Existing
vision model matching and substitution stay unchanged.

The companion advertises `llm.chat` only after listing the selected installed
model and receiving one valid scripted tool call from it. Initial probing runs
off the WebSocket read loop; connection loss and agent cancellation abort its
HTTP request and drain background work. A failed probe leaves chat unavailable.
The only ingress is the existing outbound bearer-paired
`/compute/ws` connection. No inbound bridge or local tool/shell execution exists.
The model emits tool calls for the browser to validate and execute.

The assignment contract is `type: "job.assign"`, a separate
`jobType: "LLM_CHAT_TOOLS"`, a unique `jobId`, RFC 3339 `expiresAt` in the next
five minutes, and `payload: { model, messages, tools, maxTokens }`. Unknown fields
and duplicate JSON members are rejected. Typed member names must match their
JSON spelling exactly. Arbitrary schema property names remain case-sensitive,
so `value` and `Value` can coexist. Invalid UTF-8 and unmatched UTF-16 surrogate
escapes are rejected in all JSON keys and strings before decoding; valid pairs
and literal escaped backslashes are preserved. Text messages use `system`, `user`,
`assistant` or `tool` roles; historical tool calls/results must form complete
pairs. Images use the existing vision path. `responseSchema`, streaming, and
additional provider options are unsupported in this chat contract.

Tools use OpenAI function descriptors with `strict: true`. Parameters must be
object schemas with `additionalProperties: false` on every object. The supported
schema subset is `type` (one of object, array, string, number, integer, boolean,
null), `properties`, `required`, `additionalProperties`, `items`, `enum`, `const`,
`minimum`, `maximum`, `minLength`, `maxLength`, `minItems`, `maxItems`, `description`
and `title`. Child schemas also accept exactly one supported non-null base type
paired with `"null"`, such as `["integer", "null"]`, in either order.
Present keyword operands cannot be JSON null, except for `const: null`;
null entries inside an `enum` array remain valid data.

The UI serializer's nullable wrapper is also supported: an object containing only
`anyOf`, with exactly two branches: one supported single non-null base schema and
the literal `{ "type": "null" }`, in either order. The non-null branch retains
all of its constraints. The null branch accepts the optional-field marker even
when the base branch has an `enum` or `const`. A base branch that is itself a
union, a non-literal null branch, additional wrapper siblings, or extra branches
is rejected. Bounds count the wrapper and base separately toward schema depth.

Root tool parameters remain non-nullable objects. Nullable fields still have to
be present when listed in `required`. In the type-pair form, `enum` and `const`
continue to constrain null as well as non-null values.

The companion preserves these null fields for the browser's original-domain
normalization, validation and execution permissions, including in assistant/tool
history. The valid UI serializer does not need to change. References, arbitrary
unions, patterns and general combinators remain unsupported and are refused
before inference. Returned calls must name an advertised tool and satisfy its
schema.

Numeric bounds and integer checks use exact decimal arithmetic. Numeric literals
are limited to 128 bytes, 64 mantissa digits (including zeroes), and exponent magnitude 256;
unsupported precision/range is refused. Equivalent numeric `enum`/`const`
values such as `0.1` and `0.10` compare equally. String length bounds count
decoded Unicode code points: an emoji counts once and `e` plus a combining
accent counts twice. The browser retains its original domain validation.

| Limit | Bound |
| --- | --- |
| Assignment / local request / upstream response | 64 KiB each |
| Normalized completion | 60 KiB, leaving room for the result envelope |
| Messages / tools / calls per assistant message | 64 / 20 / 8 |
| Message content / tool arguments | 16 KiB / 8 KiB |
| Tool schema depth / properties per object | 8 / 64 |
| Installed model listing | 64 KiB / 256 IDs / 200 bytes per ID |
| Explicit context / output cap | 1 to 1,048,576 / 1 to 8,192 tokens |
| Probe / inference timeout | 180 seconds each; earlier expiry or cancellation wins |

Context admission conservatively counts serialized input bytes as tokens, plus
1,024 tokens of framing allowance and the requested output cap. A request that
does not fit is refused rather than truncated. Results use the existing
`job.result` envelope with `ok`, and either a stable error code or normalized
OpenAI `model`, single `choices` entry and `usage`. Extra upstream metadata and
private reasoning fields are omitted. Raw messages, arguments and tokens are
not logged.

One shared slot handles vision and chat jobs. Cancellation, expiry, unpairing
and connection loss abort in-flight chat HTTP requests. Accepted chat job IDs
remain blocked for one hour across reconnects under the same API URL, agent ID
and pairing token. The process retains at most 4,096 tombstones and refuses new
jobs when full, rather than evicting unexpired protection. This is process-local
replay protection; it does not survive agent restart. Backend leases, ownership,
project scope and durable duplicate-result handling still belong to the backend.

Backend issue #748's chat schema/capability dispatch is a separate dependency.
This companion support and its synthetic paired-backend tests do **not** prove
real platform dispatch or an editor inspect/edit/check/save/reopen workflow.
Geometry still requires the open editor.

### Opt-in live local-model check

The live test uses a temporary test profile and the real selected local model,
with a synthetic authenticated backend. It makes no hosted calls, downloads no
models and leaves your normal configuration untouched. It is skipped unless all
three variables are supplied. On the development Mac, run through the shared
heavy-check queue (allow cold-start time):

```sh
export PATH=/Users/diandeskov/.nvm/versions/node/v22.23.2/bin:$PATH
export GOTOOLCHAIN=go1.26.0 CGO_ENABLED=0
DTX_LIVE_CHAT_URL=http://127.0.0.1:11434/v1 \
DTX_LIVE_CHAT_MODEL=gemma4:latest \
DTX_LIVE_CHAT_CONTEXT_TOKENS=131072 \
/Users/diandeskov/Documents/GitHub/Deltatronix/dtx-heavy.sh local-chat-live -- \
  go test -count=1 -v -timeout=6m -run '^TestLivePairedLocalChatTools$' .
```

This checks production installation/probing and a real model tool call through
the job handler. It sends the returned assistant call and a synthetic tool result
with that exact call ID back to the same selected model, checks the follow-up
completion acknowledges the synthetic receipt, then cancels a third accepted job.
Deterministic
HTTP fixtures separately check aborting an in-flight model request. A skipped
live test is not a successful live-model proof.

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
