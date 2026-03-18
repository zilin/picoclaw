# Guide: Using Whisper CLI with PicoClaw

This guide explains how to use the `whisper` CLI tool (from `whisper.cpp`) with `picoclaw` for local audio transcription without needing Go CGO bindings!

## Prerequisites

1.  **Install `whisper-cpp` via Homebrew**:
    ```bash
    brew install whisper-cpp
    ```
2.  **Verify `whisper-cli` is in your PATH**:
    Homebrew usually installs it. If it's not present, you can specify its absolute path in the configuration!

## Step 1: Download Models

You can download models from Hugging Face. Here is a script to download common models to your cache directory:

```bash
#!/bin/bash
mkdir -p ~/.cache/whisper

echo "Downloading base.en..."
curl -L -o ~/.cache/whisper/ggml-base.en.bin https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-base.en.bin

echo "Downloading base..."
curl -L -o ~/.cache/whisper/ggml-base.bin https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-base.bin

echo "Downloading large-v3-turbo..."
curl -L -o ~/.cache/whisper/ggml-large-v3-turbo.bin https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-large-v3-turbo.bin

echo "All downloads complete!"
```

## Step 2: Configure `picoclaw`

Update your `config.json` to enable and configure `whisper`:

```json
{
  "voice": {
    "transcriber": "whisper",
    "whisper": {
      "model_path": "~/.cache/whisper/ggml-base.en.bin",
      "cli_path": "/Users/zilin/homebrew/bin/whisper-cli"
    }
  }
}
```

-   `model_path`: Path to the downloaded model binary.
-   `cli_path`: Optional path to the `whisper-cli` binary if it's not in your global `PATH`.

## Step 3: Run the Gateway

Rebuild the gateway and run it:

```bash
make build
./build/picoclaw-darwin-arm64 gateway
```
