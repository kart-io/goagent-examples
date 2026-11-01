package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"

	"github.com/kart-io/goagent/llm"
	"github.com/kart-io/goagent/llm/providers"
)

// Capture synthetic requests locally to check prompt duplication independently
// of supplier token counts. No provider credentials or external network needed.
func TestProviderRequestPromptEquivalence(t *testing.T) {
	type capture struct {
		Provider  string
		Scenario  string
		BodyBytes int
		System    string
		User      string
		Body      map[string]any
	}
	var captures []capture
	for _, scenario := range []string{"single", "task"} {
		var openAIRequest map[string]any
		for _, provider := range []string{"minimax", "deepseek", "gemini"} {
			t.Run(scenario+"/"+provider, func(t *testing.T) {
				bodies := make(chan []byte, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, _ := io.ReadAll(r.Body)
					bodies <- body
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"error":{"code":400,"status":"INVALID_ARGUMENT","message":"local diagnostic stop"}}`))
				}))
				defer server.Close()
				opts := []llm.ClientOption{llm.WithAPIKey("local-diagnostic-only"), llm.WithBaseURL(server.URL), llm.WithModel("payload-diagnostic"), llm.WithMaxTokens(2048)}
				var c llm.Client
				var err error
				switch provider {
				case "gemini":
					c, err = providers.NewGeminiWithOptions(opts...)
				case "deepseek":
					c, err = providers.NewDeepSeekWithOptions(opts...)
				default:
					c, err = providers.NewOpenAIWithOptions(opts...)
				}
				if err != nil {
					t.Fatal(err)
				}
				result := runScenario(context.Background(), c, scenario)
				if result.Passed {
					t.Fatal("diagnostic server must stop execution")
				}
				var body []byte
				select {
				case body = <-bodies:
				default:
					t.Fatal("provider did not reach local diagnostic server")
				}
				var request map[string]any
				if err = json.Unmarshal(body, &request); err != nil {
					t.Fatalf("invalid request: %v", err)
				}
				var system, user string
				if provider == "gemini" {
					sys := request["systemInstruction"].(map[string]any)["parts"].([]any)
					contents := request["contents"].([]any)
					if len(sys) != 1 || len(contents) != 1 {
						t.Fatal("duplicated system or user content")
					}
					system = sys[0].(map[string]any)["text"].(string)
					parts := contents[0].(map[string]any)["parts"].([]any)
					if len(parts) != 1 {
						t.Fatal("duplicated user parts")
					}
					user = parts[0].(map[string]any)["text"].(string)
				} else {
					msgs := request["messages"].([]any)
					if len(msgs) != 2 || msgs[0].(map[string]any)["role"] != "system" || msgs[1].(map[string]any)["role"] != "user" {
						t.Fatal("duplicated or reordered messages")
					}
					system = msgs[0].(map[string]any)["content"].(string)
					user = msgs[1].(map[string]any)["content"].(string)
					if provider == "minimax" {
						openAIRequest = request
					} else if !reflect.DeepEqual(openAIRequest, request) {
						t.Fatal("DeepSeek and MiniMax request bodies differ beyond endpoint")
					}
				}
				captures = append(captures, capture{Provider: provider, Scenario: scenario, BodyBytes: len(body), System: system, User: user, Body: request})
			})
		}
	}
	for i := 0; i < len(captures); i += 3 {
		for j := 1; j < 3; j++ {
			if captures[i].System != captures[i+j].System || captures[i].User != captures[i+j].User {
				t.Fatal("provider prompt content differs")
			}
		}
	}
	if out := os.Getenv("GOAGENT_USAGE_PAYLOAD_OUT"); out != "" {
		data, err := json.MarshalIndent(captures, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(out, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
}
