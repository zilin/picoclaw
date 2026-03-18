package gemini_genai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"google.golang.org/genai"

	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers/common"
	"github.com/sipeed/picoclaw/pkg/providers/protocoltypes"
)

type GeminiGenAIProvider struct {
	client  *genai.Client
	backend string // "gemini-genai" or "gemini-vertex"
}

// SupportsThinking implements providers.ThinkingCapable.
func (p *GeminiGenAIProvider) SupportsThinking() bool { return true }

func NewGeminiGenAIProvider(apiKey string) (*GeminiGenAIProvider, error) {
	if apiKey == "" {
		apiKey = os.Getenv("GEMINI_API_KEY")
	}
	if apiKey == "" {
		return nil, fmt.Errorf("gemini api key is required")
	}

	client, err := genai.NewClient(context.Background(), &genai.ClientConfig{
		APIKey: apiKey,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create genai client: %w", err)
	}

	return &GeminiGenAIProvider{
		client:  client,
		backend: "gemini-genai",
	}, nil
}

func NewGeminiVertexProvider(project, location string) (*GeminiGenAIProvider, error) {
	client, err := genai.NewClient(context.Background(), &genai.ClientConfig{
		Backend:  genai.BackendVertexAI,
		Project:  project,
		Location: location,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create vertex genai client: %w", err)
	}

	return &GeminiGenAIProvider{
		client:  client,
		backend: "gemini-vertex",
	}, nil
}

func (p *GeminiGenAIProvider) Chat(
	ctx context.Context,
	messages []protocoltypes.Message,
	tools []protocoltypes.ToolDefinition,
	model string,
	options map[string]any,
) (*protocoltypes.LLMResponse, error) {
	return p.ChatStream(ctx, messages, tools, model, options, nil)
}

func (p *GeminiGenAIProvider) ChatStream(
	ctx context.Context,
	messages []protocoltypes.Message,
	tools []protocoltypes.ToolDefinition,
	model string,
	options map[string]any,
	onProgress func(partial *protocoltypes.LLMResponse),
) (*protocoltypes.LLMResponse, error) {

	// 1. Map roles to Gemini roles and normalize turn sequence
	geminiMessages := make([]protocoltypes.Message, 0, len(messages))
	var systemInstruction *genai.Content

	for _, m := range messages {
		if m.Role == "system" {
			if systemInstruction == nil {
				systemInstruction = &genai.Content{}
			}
			if m.Content != "" {
				systemInstruction.Parts = append(systemInstruction.Parts, &genai.Part{Text: m.Content})
			}
			continue
		}

		role := m.Role
		if role == "assistant" {
			role = "model"
		} else if role == "tool" {
			role = "user"
		}

		geminiMessages = append(geminiMessages, protocoltypes.Message{
			Role:       role,
			Content:    m.Content,
			Media:      m.Media,
			ToolCalls:  m.ToolCalls,
			ToolCallID: m.ToolCallID,
		})
	}

	geminiMessages = common.MergeConsecutiveRoles(geminiMessages)
	geminiMessages = common.EnsureUserStart(geminiMessages)

	if len(geminiMessages) == 0 {
		return nil, fmt.Errorf("no valid content turns to send to Gemini (must start with user)")
	}

	// 2. Convert normalized messages to genai.Content
	var contents []*genai.Content
	toolCallNames := make(map[string]string)

	for _, m := range geminiMessages {
		content := &genai.Content{
			Role: m.Role,
		}

		// Handle Text
		if m.Content != "" && m.ToolCallID == "" {
			content.Parts = append(content.Parts, &genai.Part{
				Text: m.Content,
			})
		}

		// Handle Images/Media
		for _, mediaRef := range m.Media {
			if strings.HasPrefix(mediaRef, "data:") {
				parts := strings.SplitN(mediaRef, ",", 2)
				if len(parts) == 2 {
					header := parts[0]
					data := parts[1]

					mimeType := "image/jpeg"
					if idx := strings.Index(header, ":"); idx >= 0 {
						if endIdx := strings.Index(header[idx:], ";"); endIdx >= 0 {
							mimeType = header[idx+1 : idx+endIdx]
						}
					}

					imgBytes, err := base64.StdEncoding.DecodeString(data)
					if err == nil {
						content.Parts = append(content.Parts, &genai.Part{
							InlineData: &genai.Blob{
								Data:     imgBytes,
								MIMEType: mimeType,
							},
						})
					}
				}
			} else if strings.HasPrefix(mediaRef, "/") || strings.HasPrefix(mediaRef, "~") {
				imgBytes, err := os.ReadFile(mediaRef)
				if err == nil {
					mimeType := "image/jpeg"
					if strings.HasSuffix(mediaRef, ".png") {
						mimeType = "image/png"
					}
					content.Parts = append(content.Parts, &genai.Part{
						InlineData: &genai.Blob{
							Data:     imgBytes,
							MIMEType: mimeType,
						},
					})
				}
			}
		}

		// Handle ToolCalls for assistant/model role
		for _, tc := range m.ToolCalls {
			toolName, toolArgs, thoughtSignature := normalizeStoredToolCall(tc)
			if toolName != "" {
				if tc.ID != "" {
					toolCallNames[tc.ID] = toolName
				}
				content.Parts = append(content.Parts, &genai.Part{
					FunctionCall: &genai.FunctionCall{
						Name: toolName,
						Args: toolArgs,
					},
				})
			}
			if thoughtSignature != "" {
				sigBytes, err := base64.StdEncoding.DecodeString(thoughtSignature)
				if err == nil {
					content.Parts = append(content.Parts, &genai.Part{
						ThoughtSignature: sigBytes,
					})
				}
			}
		}

		// Handle Tool Results (tool role or user role with ToolCallID)
		if (m.Role == "tool" || m.Role == "user") && m.ToolCallID != "" {
			toolName := resolveToolResponseName(m.ToolCallID, toolCallNames)
			if toolName != "" {
				content.Parts = append(content.Parts, &genai.Part{
					FunctionResponse: &genai.FunctionResponse{
						Name: toolName,
						Response: map[string]any{
							"result": m.Content,
						},
					},
				})
			}
		}

		if len(content.Parts) > 0 {
			contents = append(contents, content)
		}
	}

	// 2. Convert tools to genai.Tool
	var genaiTools []*genai.Tool
	for _, t := range tools {
		if t.Type == "function" {
			genaiTools = append(genaiTools, &genai.Tool{
				FunctionDeclarations: []*genai.FunctionDeclaration{
					{
						Name:        t.Function.Name,
						Description: t.Function.Description,
						Parameters:  mapToGenaiSchema(t.Function.Parameters),
					},
				},
			})
		}
	}

	// 3. Make the call
	config := &genai.GenerateContentConfig{
		Tools:             genaiTools,
		SystemInstruction: systemInstruction,
	}

	// Apply options (temperature, etc.)
	if temp, ok := options["temperature"].(float64); ok {
		t32 := float32(temp)
		config.Temperature = &t32
	}
	if maxTokens, ok := options["max_tokens"].(int); ok {
		config.MaxOutputTokens = int32(maxTokens)
	}

	// Extended Thinking
	if level, ok := options["thinking_level"].(string); ok && level != "" && level != "off" {
		config.ThinkingConfig = &genai.ThinkingConfig{
			IncludeThoughts: true,
		}
		// Set either Level OR Budget, never both
		switch strings.ToLower(level) {
		case "low":
			config.ThinkingConfig.ThinkingLevel = "LOW"
		case "medium":
			config.ThinkingConfig.ThinkingLevel = "MEDIUM"
		case "high", "xhigh":
			config.ThinkingConfig.ThinkingLevel = "HIGH"
		default:
			// If not a named level, fallback to budget mapping
			budget := int32(levelToBudget(level))
			if budget > 0 {
				config.ThinkingConfig.ThinkingBudget = &budget
			}
		}

		logger.InfoCF("gemini_genai", "Thinking enabled", map[string]any{
			"level": level,
			"model": model,
		})
	}

	// Use streaming for better responsiveness
	return p.chatStreaming(ctx, model, contents, config, onProgress)
}

// levelToBudget maps a thinking level to budget_tokens for Gemini.
func levelToBudget(level string) int {
	switch strings.ToLower(level) {
	case "low":
		return 4096
	case "medium":
		return 16384
	case "high":
		return 32000
	case "xhigh":
		return 64000
	default:
		return 0
	}
}

func (p *GeminiGenAIProvider) chatStreaming(
	ctx context.Context,
	model string,
	contents []*genai.Content,
	config *genai.GenerateContentConfig,
	onProgress func(partial *protocoltypes.LLMResponse),
) (*protocoltypes.LLMResponse, error) {
	llmResp := &protocoltypes.LLMResponse{}

	for resp, err := range p.client.Models.GenerateContentStream(ctx, model, contents, config) {
		if err != nil {
			return nil, fmt.Errorf("gemini streaming error: %w", err)
		}

		if resp == nil || len(resp.Candidates) == 0 {
			continue
		}

		hasNewContent := false
		cand := resp.Candidates[0]
		if cand.Content != nil {
			for _, part := range cand.Content.Parts {
				if part.Thought {
					llmResp.Reasoning += part.Text
					llmResp.ReasoningContent += part.Text
					hasNewContent = true
				}
				if part.Text != "" && !part.Thought {
					llmResp.Content += part.Text
					hasNewContent = true
				}
				if part.FunctionCall != nil {
					fc := part.FunctionCall
					callArgsJson, _ := json.Marshal(fc.Args)
					toolCall := protocoltypes.ToolCall{
						Type: "function",
						Function: &protocoltypes.FunctionCall{
							Name:      fc.Name,
							Arguments: string(callArgsJson),
						},
					}
					// Extract thought signature if present
					if part.ThoughtSignature != nil {
						sig := base64.StdEncoding.EncodeToString(part.ThoughtSignature)
						toolCall.ThoughtSignature = sig
						toolCall.Function.ThoughtSignature = sig
						toolCall.ExtraContent = &protocoltypes.ExtraContent{
							Google: &protocoltypes.GoogleExtra{
								ThoughtSignature: sig,
							},
						}
					}
					llmResp.ToolCalls = append(llmResp.ToolCalls, toolCall)
					hasNewContent = true
				}
			}
		}

		if cand.FinishReason != "" {
			llmResp.FinishReason = string(cand.FinishReason)
		}

		if resp.UsageMetadata != nil {
			llmResp.Usage = &protocoltypes.UsageInfo{
				PromptTokens:     int(resp.UsageMetadata.PromptTokenCount),
				CompletionTokens: int(resp.UsageMetadata.CandidatesTokenCount),
				TotalTokens:      int(resp.UsageMetadata.TotalTokenCount),
			}
		}

		if hasNewContent && onProgress != nil {
			onProgress(llmResp)
		}
	}

	llmResp.FinishReason = normalizeFinishReason(llmResp.FinishReason)
	if len(llmResp.ToolCalls) > 0 {
		llmResp.FinishReason = "tool_calls"
	}

	return llmResp, nil
}

func normalizeFinishReason(reason string) string {
	switch strings.ToUpper(reason) {
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "OTHER":
		return "stop"
	case "MALFORMED_FUNCTION_CALL":
		return "tool_calls"
	default:
		// Many OpenAI-compatible providers and some SDK versions use 'stop'
		if reason == "" {
			return "stop"
		}
		return strings.ToLower(reason)
	}
}

func (p *GeminiGenAIProvider) GetDefaultModel() string {
	return "gemini-3-flash"
}

func mapToGenaiSchema(params map[string]any) *genai.Schema {
	if params == nil {
		return nil
	}

	typ, _ := params["type"].(string)
	description, _ := params["description"].(string)
	schema := &genai.Schema{
		Description: description,
	}

	switch typ {
	case "string":
		schema.Type = genai.TypeString
	case "number":
		schema.Type = genai.TypeNumber
	case "integer":
		schema.Type = genai.TypeInteger
	case "boolean":
		schema.Type = genai.TypeBoolean
	case "object":
		schema.Type = genai.TypeObject
		if props, ok := params["properties"].(map[string]any); ok {
			schema.Properties = make(map[string]*genai.Schema)
			for k, v := range props {
				if m, ok := v.(map[string]any); ok {
					schema.Properties[k] = mapToGenaiSchema(m)
				}
			}
		}
		if req, ok := params["required"].([]any); ok {
			for _, r := range req {
				if s, ok := r.(string); ok {
					schema.Required = append(schema.Required, s)
				}
			}
		}
	case "array":
		schema.Type = genai.TypeArray
		if items, ok := params["items"].(map[string]any); ok {
			schema.Items = mapToGenaiSchema(items)
		}
	}

	return schema
}

func normalizeStoredToolCall(tc protocoltypes.ToolCall) (string, map[string]any, string) {
	name := tc.Name
	args := tc.Arguments
	thoughtSignature := tc.ThoughtSignature

	if name == "" && tc.Function != nil {
		name = tc.Function.Name
		if thoughtSignature == "" {
			thoughtSignature = tc.Function.ThoughtSignature
		}
	}

	if args == nil {
		args = map[string]any{}
	}

	if len(args) == 0 && tc.Function != nil && tc.Function.Arguments != "" {
		var parsed map[string]any
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &parsed); err == nil && parsed != nil {
			args = parsed
		}
	}

	return name, args, thoughtSignature
}

func resolveToolResponseName(toolCallID string, toolCallNames map[string]string) string {
	if toolCallID == "" {
		return ""
	}

	if name, ok := toolCallNames[toolCallID]; ok && name != "" {
		return name
	}

	// Fallback to infer from call ID if not found in map
	return inferToolNameFromCallID(toolCallID)
}

func inferToolNameFromCallID(toolCallID string) string {
	if !strings.HasPrefix(toolCallID, "call_") {
		return toolCallID
	}

	rest := strings.TrimPrefix(toolCallID, "call_")
	// Expected format: call_toolname_timestamp
	if idx := strings.LastIndex(rest, "_"); idx > 0 {
		candidate := rest[:idx]
		if candidate != "" {
			return candidate
		}
	}

	return toolCallID
}
