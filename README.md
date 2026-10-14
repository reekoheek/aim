# aim

AI Management Server — lightweight LLM proxy with multi-model support. Forwards requests to OpenAI-compatible APIs.

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

providers:
  banyu-minipc:
    base_url: http://banyu-minipc:8080
    api_key: sk-xxx
  openai:
    base_url: https://api.openai.com/v1
    api_key: sk-your-key

server:
  host: 0.0.0.0
  port: 8080
  save_slot_path: /var/cache/aim/slots # optional, default: <user cache dir>/aim/slots
  slot_expiration: 1h                  # optional, default: 1h
  slot_max_files: 3                    # optional, default: 3
  idle_timeout: 300                    # optional, seconds of inactivity before aim stops llama-server (default 300 = 5m, 0 = never)
```

Each session's llama-server KV cache is saved as a `.bin` file under `save_slot_path` when another session takes over the slot (or llama-server stops), and restored when that session comes back. A file counts as used when it is saved or when its session is requested again:

- files unused for longer than `slot_expiration` (a Go duration such as `1h`, `90m`) are deleted
- only the `slot_max_files` most recently used files are kept; older ones are deleted right after a session switch

Both limits are also applied on startup and then periodically (every `slot_expiration`, at most hourly).

### Local llama-server models

A `llama-server` model is started on demand (one at a time). In its `flags`, every key that starts with `-` is a llama-server flag written exactly as on the command line, and is passed as-is:

- `-hf: org/repo` → `-hf org/repo`, `--temp: 0.6` → `--temp 0.6`
- `true` adds a switch on its own (`--jinja: true` → `--jinja`), `false` leaves it out; `-fa: on` stays `-fa on`
- flags are passed sorted by name; when a flag appears twice (e.g. `-c` and `--ctx-size`), llama-server uses the last one

Keys without a dash are aim settings:

- `inherit: <template>` starts from a template under `templates`; the model's flags and settings win
- `port`, `url` — where aim runs and reaches llama-server
- `stop-timeout` — seconds aim waits for llama-server to stop before killing it (default 30)

`--host`, `--port`, `--log-colors` and `--slot-save-path` are always set by aim.

```yaml
templates:
  default:
    -ngl: 999
    -fa: on
    -c: 128000
    --jinja: true
    --offline: true

models:
  qwen3.6-35b:
    type: llama-server
    bin: /path/to/llama-server   # optional, default: llama-server from PATH
    stop_timeout: 10             # optional, seconds before kill (default 30)
    flags:
      inherit: default
      --spec-type: draft-mtp
      -hf: byteshape/Qwen3.6-35B-A3B-MTP-GGUF
      -hff: Qwen3.6-35B-A3B-IQ4_XS-4.19bpw.gguf
      --temp: 0.6
      --top-k: 20
```

llama-server itself also applies the `[*]` section of `~/.config/llama.cpp/config.ini` (or `$XDG_CONFIG_HOME/llama.cpp/config.ini`, `/etc/llama.cpp/config.ini`) to every run, including the ones aim starts.

## Usage

### Proxy endpoint

```bash
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model": "banyu-minipc/qwen3.6-35b", "messages": [{"role": "user", "content": "Hello!"}]}'
```

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
