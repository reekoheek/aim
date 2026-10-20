# aim

AI Management Server — lightweight LLM proxy with multi-model support. Forwards requests to OpenAI- and Anthropic-compatible APIs and to llama.cpp's llama-server.

## Quick Start

```bash
go build -o bin/aim-server ./cmd/aim-server/
./bin/aim-server          # loads .aim.yaml from CWD
./bin/aim-server -c path/to/config.yaml
```

## Configuration

Create `.aim.yaml` in your working directory:

```yaml
models:
  banyu-minipc/qwen3.6-35b:
    type: chat-completion
    provider: banyu-minipc
    model: qwen3.6-35b
  openai/gpt-4:
    type: chat-completion
    provider: openai
    model: gpt-4
  banyu-minipc/julia-1:
    type: systemone
    provider: banyu-minipc
    model: julia-1
  anthropic/claude-sonnet:
    type: anthropic
    provider: anthropic
    model: claude-sonnet-5-5

providers:
  banyu-minipc:
    base_url: http://banyu-minipc:8080
    api_key: sk-xxx
  openai:
    base_url: https://api.openai.com
    api_key: sk-your-key
  anthropic:
    base_url: https://api.anthropic.com
    api_key: sk-ant-your-key

server:
  host: 0.0.0.0
  port: 8080
  save_slot_path: /var/cache/aim/slots # optional, default: <user cache dir>/aim/slots
  slot_expiration: 1h                  # optional, default: 1h
  slot_max_files: 3                    # optional, default: 3
  idle_timeout: 300                    # optional, seconds of inactivity before aim stops llama-server (default 300 = 5m, 0 = never)
```

Each llama-server model runs with `slots` KV cache slots (see below). A session (the `X-Session-Affinity` header, else Claude Code's `X-Claude-Code-Session-Id`; requests with neither share one session; a request that also carries Claude Code's `X-Claude-Code-Agent-Id` is a session of its own, so each subagent gets its own slot) keeps its slot while it is in use. A session that has no slot takes an empty one, or else the least recently used free slot: that slot's session is saved as a `.bin` file under `save_slot_path`, and the incoming session is restored from its file. Every unsaved slot is also saved when llama-server stops. Requests wait while every slot is busy; a request whose session already holds a busy slot waits for that slot and is served before other waiting requests. A file counts as used when it is saved or when its session is requested again:

- files unused for longer than `slot_expiration` (a Go duration such as `1h`, `90m`) are deleted
- only the `slot_max_files` most recently used files are kept; older ones are deleted right after a session switch

Both limits are also applied on startup and then periodically (every `slot_expiration`, at most hourly).

### Local llama-server models

A `llama-server` model is started on demand (one at a time). In its `flags`, every key that starts with `-` is a llama-server flag written exactly as on the command line, and is passed as-is:

- `-hf: org/repo` → `-hf org/repo`, `--temp: 0.6` → `--temp 0.6`
- `true` adds a switch on its own (`--jinja: true` → `--jinja`), `false` leaves it out; `-fa: on` stays `-fa on`
- flags are passed sorted by name; when a flag appears twice (e.g. `-b` and `--batch-size`), llama-server uses the last one

Keys without a dash are aim settings:

- `inherit: <template>` starts from a template under `templates`; the model's flags and settings win
- `port`, `url` — where aim runs and reaches llama-server

Next to `flags`, a model also takes `bin` (default: `llama-server` from `PATH`) and `stop_timeout` (seconds aim waits for llama-server to stop before killing it, default 30).

`slots` and `ctx`, also next to `flags`, size the model's slots:

- `slots: N` passes `-np N` and keeps a slot cache of N slots (default 1)
- `slots: 0` passes `-np 1` and turns the slot cache off: no `--slot-save-path`, nothing saved or restored
- `ctx: T` is the context of each slot: aim passes `-c T×N`, so every session gets `T` tokens however many slots there are; KV memory grows with `T×N`
- without `ctx` aim passes no `-c`: llama-server gives every slot the model's training context, shrinking it (to no less than 4096 per slot) when it does not fit while keeping 1 GiB of device memory free; with `-fit: off` in `flags` it splits the training context between the slots instead

aim sets `-np`, `-c`, `--host`, `--port`, `--log-colors`, `--slot-save-path`, `--offline`, `--no-ui`, `--cors-origins` and `--no-cors-credentials` itself. Writing any of them in `flags`, or an alias such as `--parallel`, `--ctx-size`, `--ui`, `--webui`, `--no-webui` or `--cors-credentials`, stops aim at startup with an error. So do `--kv-unified-per-slot` (use `ctx`) and `-kvu`/`--kv-unified`/`-no-kvu`/`--no-kv-unified`: with a unified KV pool llama-server purges idle slots to make room, which throws away the sessions aim keeps in them.

```yaml
templates:
  default:
    -ngl: 999
    -fa: on
    --jinja: true

models:
  qwen3.6-35b:
    type: llama-server
    bin: /path/to/llama-server   # optional, default: llama-server from PATH
    stop_timeout: 10             # optional, seconds before kill (default 30)
    slots: 2                     # optional, parallel slots (default 1, 0 = no slot cache)
    ctx: 128000                  # optional, context per slot; aim passes -c 256000
    flags:
      inherit: default
      --spec-type: draft-mtp
      -hf: byteshape/Qwen3.6-35B-A3B-MTP-GGUF
      -hff: Qwen3.6-35B-A3B-IQ4_XS-4.19bpw.gguf
      --temp: 0.6
      --top-k: 20
```

llama-server itself also applies the `[*]` section of `~/.config/llama.cpp/config.ini` (or `$XDG_CONFIG_HOME/llama.cpp/config.ini`, `/etc/llama.cpp/config.ini`) to every run, including the ones aim starts.

`chat-completion` models are served on `/v1/chat/completions`, `systemone` models on `/v1/systemone` and `anthropic` models on `/v1/messages`; all three forward to the provider's `base_url` with the provider's `api_key` and the entry's `model`. A `llama-server` model is served on every endpoint (see below). The query string is passed upstream as-is, and so are the upstream's response headers.

## Usage

### Proxy endpoint

```bash
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model": "banyu-minipc/qwen3.6-35b", "messages": [{"role": "user", "content": "Hello!"}]}'
```

### Decision endpoint

`/v1/systemone` serves llama.cpp decision models (Julia-1, Kev-4B, …):

```bash
curl -X POST http://localhost:8080/v1/systemone \
  -H "Content-Type: application/json" \
  -d '{"model": "julia-1", "state": "Customer: I was charged twice.", "questions": {"route": {"type": "choice", "instructions": "Which team?", "criteria": {"billing": "payments", "shipping": "delivery"}}}}'
```

- `model` and a non-empty `questions` object are required; everything else is passed to llama-server as-is. There is no streaming.
- the model must be `type: systemone` (forwarded to the provider) or `type: llama-server`; a `chat-completion` or `anthropic` model is rejected, and so is a `systemone` model on `/v1/chat/completions`
- a `llama-server` decision model is a normal `llama-server` entry and takes turns with the chat models (one llama-server at a time). The slot cache is not used for it: llama-server picks the slots for decision requests itself, so `X-Session-Affinity` is ignored and nothing is saved or restored, and `slots: N` only sets how many questions run in parallel. A `llama-server` model that is not a decision model answers 501.

```yaml
models:
  julia-1:
    type: llama-server
    slots: 8
    ctx: 2048
    flags:
      -hf: ggml-org/Julia-1-GGUF:Q8_0
      -ngl: 999
```

### Messages endpoint

`/v1/messages` and `/v1/messages/count_tokens` serve the Anthropic Messages API:

```bash
curl -X POST http://localhost:8080/v1/messages \
  -H "Content-Type: application/json" \
  -H "anthropic-version: 2023-06-01" \
  -d '{"model": "anthropic/claude-sonnet", "max_tokens": 1024, "messages": [{"role": "user", "content": "Hello!"}]}'
```

- `model` is required; the body, the query string and every header (`anthropic-version`, `anthropic-beta`, …) are passed as-is
- an `anthropic` model forwards to the provider's `base_url` + `/v1/messages` (or `/v1/messages/count_tokens`) with the entry's `model`; aim sends the provider's `api_key` as `x-api-key` and drops the client's `Authorization`
- a `llama-server` model forwards to llama-server's own `/v1/messages` with the same slot cache as chat requests; `count_tokens` starts the model when needed but takes no slot. llama.cpp drops `id_slot` on `/v1/messages` until [ggml-org/llama.cpp#28554](https://github.com/ggml-org/llama.cpp/pull/28554) is merged, so with `slots` above 1 llama-server picks the slot itself and the slot cache saves and restores the wrong sessions more often (replies stay correct, they are just computed again). With `slots: 1` there is only one slot and the cache works fully.
- `chat-completion` and `systemone` models are rejected, and so is an `anthropic` model on `/v1/chat/completions` and `/v1/systemone`
- errors aim writes itself use Anthropic's shape, `{"type":"error","error":{"type":"invalid_request_error" or "api_error","message":"…"}}`; errors from the provider or from llama-server (which answers in OpenAI's shape) are passed through unchanged

### With Claude Code

```bash
export ANTHROPIC_BASE_URL=http://localhost:8080
export ANTHROPIC_API_KEY=any-value              # aim sends the provider's key itself
export ANTHROPIC_MODEL=qwen3.6-35b               # an aim model selector
export ANTHROPIC_DEFAULT_HAIKU_MODEL=qwen3.6-35b
export CLAUDE_CODE_MAX_CONTEXT_TOKENS=128000     # the model's ctx; Claude Code assumes 200000 otherwise
export CLAUDE_CODE_MAX_OUTPUT_TOKENS=16000
claude
```

- Claude Code asks for `ANTHROPIC_MODEL` and, for background calls, `ANTHROPIC_DEFAULT_HAIKU_MODEL`. Point both at the same `llama-server` model: aim runs one llama-server at a time, so a different haiku model makes it switch models for every background call.
- Claude Code compacts the conversation only on Anthropic's "prompt is too long" error, which llama-server does not send. Set `CLAUDE_CODE_MAX_CONTEXT_TOKENS` to the model's `ctx` so it compacts before the context is full (`CLAUDE_CODE_AUTO_COMPACT_WINDOW` moves that point, 100000 at the lowest), and keep `CLAUDE_CODE_MAX_OUTPUT_TOKENS` well below `ctx`.
- with an `anthropic` provider that rejects Anthropic's newest beta fields, set `CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1`

### With OpenAI SDK (Python)

```python
from openai import OpenAI

client = OpenAI(base_url="http://localhost:8080/v1", api_key="any-value")
response = client.chat.completions.create(model="banyu-minipc/qwen3.6-35b", messages=[{"role": "user", "content": "Hello!"}], stream=True)
```

## Platform

Native support for **Linux**, **macOS**, and **Windows**. Pure Go, no `cgo`, no OS-specific dependencies.

## CLI Flags

| Flag | Description |
|------|-------------|
| `-c, --config` | Config file path (default: `.aim.yaml` in CWD) |
| `-v, --version` | Show version |
| `-h, --help` | Show help |

## Structure

```
aim/
├── cmd/aim-server/main.go          # Entry point
├── pkg/config/config.go            # YAML config loader
├── pkg/proxy/proxy.go              # HTTP handler + streaming
└── internal/logger/logger.go       # Logging utility
```
