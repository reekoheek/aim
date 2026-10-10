# aim

Lightweight LLM proxy server in Go. Forwards requests to OpenAI-compatible APIs with multi-model support.

## Quick Start

```bash
# Build
go build -o bin/aim ./cmd/aim/

# Run (auto-loads .aim.yaml from CWD)
./bin/aim

# Run with custom config
./bin/aim -c path/to/config.yaml
```

## Configuration

Create `.aim.yaml` in your working directory:

```yaml
models:
  banyu-minipc/qwen3.6-35b:
    type: provider
    provider: banyu-minipc
    model: qwen3.6-35b
  openai/gpt-4:
    type: provider
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
```

## Usage

### Proxy endpoint

```bash
# Non-streaming
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "banyu-minipc/qwen3.6-35b",
    "messages": [{"role": "user", "content": "Hello!"}]
  }'

# Streaming
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -N -d '{
    "model": "banyu-minipc/qwen3.6-35b",
    "messages": [{"role": "user", "content": "Hello!"}],
    "stream": true
  }'
```

### With OpenAI SDK (Python)

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:8080/v1",
    api_key="any-value"  # not used, proxy handles auth
)

response = client.chat.completions.create(
    model="banyu-minipc/qwen3.6-35b",
    messages=[{"role": "user", "content": "Hello!"}],
    stream=True
)

for chunk in response:
    if chunk.choices[0].delta.content:
        print(chunk.choices[0].delta.content, end="")
```

## CLI Flags

| Flag | Description |
|------|-------------|
| `-c, --config` | Config file path (default: `.aim.yaml` in CWD) |
| `-v, --version` | Show version |
| `-h, --help` | Show help |

## Logging

Logs are human-readable with timestamp, level, message, and attributes:

```
[2026-10-11T01:03:17.912] [INFO] request method=POST path=/v1/chat/completions model=banyu-minipc/qwen3.6-35b remote_addr=[::1]:38464
[2026-10-11T01:03:17.923] [INFO] response status=200 duration=1.234s
```

## Structure

```
aim/
├── go.mod
├── .aim.yaml
├── cmd/aim/main.go          # Entry point (cobra CLI)
├── pkg/config/config.go     # YAML config loader
├── pkg/proxy/proxy.go       # HTTP handler + streaming
└── internal/logger/logger.go # Logging utility
```
