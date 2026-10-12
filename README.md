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
