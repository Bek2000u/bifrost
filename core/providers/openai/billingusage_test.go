package openai

import (
	"github.com/maximhq/bifrost/core/schemas"
	"testing"
)

func TestBillingChatStreamUnknownVersusMeasuredZero(t *testing.T) {
	for _, known := range []bool{false, true} {
		name := "unknown"
		usage := ""
		if known {
			name = "measured-zero"
			usage = "data: {\"id\":\"s\",\"object\":\"chat.completion.chunk\",\"model\":\"test\",\"choices\":[],\"usage\":{\"prompt_tokens\":0,\"completion_tokens\":0,\"total_tokens\":0}}\n\n"
		}
		t.Run(name, func(t *testing.T) {
			stop := "stop"
			server := completeSSEServer(t, chatChunk("hello", nil)+chatChunk("", &stop)+usage+"data: [DONE]\n\n")
			defer server.Close()
			ctx := newStreamTestContext()
			ctx.SetValue(schemas.BifrostContextKeyStreamAccumulatedUsage, &schemas.BifrostLLMUsage{TotalTokens: 99})
			provider := newStreamTestProvider(server.URL)
			stream, err := provider.ChatCompletionStream(ctx, passthroughPostHook, nil, testKey(), basicChatRequest())
			if err != nil {
				t.Fatal(err)
			}
			chunks := collectChunks(t, stream)
			if len(chunks) == 0 {
				t.Fatal("no chunks")
			}
			last := chunks[len(chunks)-1]
			if last.BifrostError != nil || last.BifrostChatResponse == nil {
				t.Fatal(last)
			}
			if (last.BifrostChatResponse.Usage != nil) != known {
				t.Fatalf("usage presence: got %+v, known %v", last.BifrostChatResponse.Usage, known)
			}
			if known && last.BifrostChatResponse.Usage.TotalTokens != 0 {
				t.Fatal("stale usage from a previous attempt was retained")
			}
			if (ctx.Value(schemas.BifrostContextKeyStreamAccumulatedUsage) != nil) != known {
				t.Fatal("unobserved partial billing usage was synthesized")
			}
		})
	}
}
