# Gemini Thinking & Reasoning Support

PicoClaw supports the explicit reasoning (thinking) process for Gemini 2.0 and Gemini 3 models. This allows you to see the model's internal chain of thought before it executes tools or provides a final answer.

## Enabling Reasoning

To enable reasoning, you must configure a model from the `gemini-genai` or `gemini-vertex` providers in your `model_list` and set the `thinking_level`.

### Configuration (config.json)

```json
{
  "model_list": [
    {
      "model_name": "gemini-3-think",
      "model": "gemini-genai/gemini-3-flash",
      "api_key": "YOUR_API_KEY",
      "thinking_level": "medium",
      "max_tokens": 16000
    }
  ]
}
```

### Supported `thinking_level` values:
*   `off`: (Default) Disables the thinking process.
*   `low`: Minimal reasoning, faster response.
*   `medium`: Balanced reasoning and speed.
*   `high` / `xhigh`: Deep reasoning for complex tasks.

## Provider Differences

### 1. gemini-genai (Google AI SDK)
*   **Prefix**: `gemini-genai/`
*   **Requirements**: An API key from [Google AI Studio](https://aistudio.google.com/).
*   **Models**: `gemini-3-flash`, `gemini-2.0-flash-thinking-exp-01-21`.

### 2. gemini-vertex (Google Cloud Vertex AI)
*   **Prefix**: `gemini-vertex/`
*   **Requirements**: `api_base` set to `project_id:location` (e.g., `my-project:us-central1`).
*   **Models**: `gemini-3-flash-preview`, `gemini-2.0-flash-thinking-exp`.

## Visualizing Thoughts in Google Chat

When using the Google Chat channel, you can choose where the reasoning appears:

1.  **Main Thread**: If `reasoning_channel_id` is empty, thoughts will appear in the main chat bubble and be replaced by the final answer.
2.  **Dedicated Thread**: Set `reasoning_channel_id` to a specific thread URL (e.g., `spaces/AAAA/threads/BBBB`) to see the thoughts live in a separate side-bar or space.

### Intermediate Tool Results
For external channels like Google Chat, PicoClaw automatically redirects large intermediate tool outputs (like raw code execution results) to the reasoning thread to keep the main conversation clean.

## Disabling Reasoning
To disable reasoning for a specific model, either remove the `thinking_level` field or set it to `"off"`.

```json
{
  "model_name": "gemini-fast",
  "model": "gemini-genai/gemini-2.0-flash",
  "thinking_level": "off"
}
```
