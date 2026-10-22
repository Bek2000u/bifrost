package fastgen

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/maximhq/bifrost/core/schemas"
)

func ToFastgenChatRequest(request *schemas.BifrostChatRequest) (*FastgenPromptRequest, error) {
	if request == nil || request.Model != "prompt-generator" || len(request.Input) != 1 || request.Input[0].Role != schemas.ChatMessageRoleUser || request.Input[0].Content == nil {
		return nil, fmt.Errorf("fastgen prompt-generator requires one user text message")
	}
	if p := request.Params; p != nil {
		if len(p.Tools) > 0 || p.ToolChoice != nil || p.ResponseFormat != nil || p.N != nil && *p.N != 1 || len(p.ExtraParams) > 0 {
			return nil, fmt.Errorf("fastgen prompt-generator does not support tools, structured output or extra parameters")
		}
	}
	content := request.Input[0].Content
	var prompt string
	if content.ContentStr != nil {
		prompt = *content.ContentStr
	} else {
		var builder strings.Builder
		for _, block := range content.ContentBlocks {
			if block.Type != schemas.ChatContentBlockTypeText || block.Text == nil {
				return nil, fmt.Errorf("fastgen prompt-generator accepts text only")
			}
			builder.WriteString(*block.Text)
		}
		prompt = builder.String()
	}
	if count := utf8.RuneCountInString(prompt); count < 1 || count > 10000 {
		return nil, fmt.Errorf("fastgen user_prompt must contain 1 to 10000 characters")
	}
	return &FastgenPromptRequest{UserPrompt: prompt}, nil
}

func ToBifrostChatResponse(native *FastgenPromptResponse, model string) (*schemas.BifrostChatResponse, error) {
	if native.OperationID == "" || native.GeneratedText == nil || native.Success != nil && !*native.Success || native.Usage == nil || native.Usage.PromptTokens == nil || native.Usage.CompletionTokens == nil || *native.Usage.PromptTokens < 0 || *native.Usage.CompletionTokens < 0 {
		return nil, fmt.Errorf("fastgen omitted final prompt receipt or exact token usage")
	}
	usage := &schemas.BifrostLLMUsage{PromptTokens: *native.Usage.PromptTokens, CompletionTokens: *native.Usage.CompletionTokens, TotalTokens: *native.Usage.PromptTokens + *native.Usage.CompletionTokens}
	if native.Usage.TotalTokens != nil && *native.Usage.TotalTokens != usage.TotalTokens {
		return nil, fmt.Errorf("fastgen token total does not match usage")
	}
	return &schemas.BifrostChatResponse{ID: native.OperationID, Model: model, Object: "chat.completion", Usage: usage, Choices: []schemas.BifrostResponseChoice{{Index: 0, FinishReason: schemas.Ptr("stop"), ChatNonStreamResponseChoice: &schemas.ChatNonStreamResponseChoice{Message: &schemas.ChatMessage{Role: schemas.ChatMessageRoleAssistant, Content: &schemas.ChatMessageContent{ContentStr: native.GeneratedText}}}}}}, nil
}
