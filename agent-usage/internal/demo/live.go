package demo

import (
	"fmt"
	"github.com/kart-io/goagent/core/usage"
	"github.com/kart-io/goagent/llm"
	"github.com/kart-io/goagent/llm/providers"
	"os"
	"strings"
	"time"
)

// NewLiveClient requires real credentials and never falls back to a mock.
func NewLiveClient(provider, model string, configs ...usage.ReasoningConfig) (llm.Client, error) {
	prefix := strings.ToUpper(provider)
	key := os.Getenv(prefix + "_API_KEY")
	if key == "" {
		return nil, fmt.Errorf("%s_API_KEY is required; no mock fallback", prefix)
	}
	opts := []llm.ClientOption{llm.WithAPIKey(key), llm.WithModel(model), llm.WithMaxTokens(2048), llm.WithTimeout(60 * time.Second)}
	if len(configs) > 0 {
		opts = append(opts, llm.WithReasoning(configs[0]))
	}
	endpoint := os.Getenv(prefix + "_BASE_URL")
	switch provider {
	case "gemini":
		if endpoint != "" {
			opts = append(opts, llm.WithBaseURL(endpoint))
		}
		return providers.NewGeminiWithOptions(opts...)
	case "deepseek":
		if endpoint != "" {
			opts = append(opts, llm.WithBaseURL(endpoint))
		}
		return providers.NewDeepSeekWithOptions(opts...)
	case "minimax":
		if endpoint == "" {
			endpoint = "https://api.minimax.io/v1"
		}
		opts = append(opts, llm.WithBaseURL(endpoint))
		return providers.NewOpenAIWithOptions(opts...)
	default:
		return nil, fmt.Errorf("provider must be deepseek, minimax, or gemini")
	}
}
