package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kart-io/goagent/codec/json"
	"github.com/kart-io/goagent/constants"
	"github.com/kart-io/goagent/core/durable"
	"github.com/kart-io/goagent/llm"
	"github.com/stretchr/testify/require"
)

type fixtureModel struct{}

func (fixtureModel) Provider() constants.Provider { return constants.ProviderCustom }
func (fixtureModel) CompleteStream(_ context.Context, req *llm.CompletionRequest) (<-chan *llm.StreamChunk, error) {
	if req.Model != "fixture" || req.MaxTokens != 2048 {
		return nil, fmt.Errorf("模型配置未传递")
	}
	ch := make(chan *llm.StreamChunk, 2)
	ch <- &llm.StreamChunk{Delta: liveMarker + " 合成摘要"}
	ch <- &llm.StreamChunk{Done: true, FinishReason: "stop", Usage: &llm.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}}
	close(ch)
	return ch, nil
}

type liveCaseEvidence struct {
	Scenario scenarioResult `json:"scenario"`
	Requests int64          `json:"provider_request_delta"`
}

// 同一套断言用于离线接线验证和显式开启的真实模型验收。
func verifyModelProtocols(t *testing.T, backend *modelBackend) []liveCaseEvidence {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	app, err := startApplication(ctx, "127.0.0.1:0", "model-test-token", backend)
	require.NoError(t, err)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, app.Close(ctx))
	}()
	evidence := []liveCaseEvidence{}
	for _, name := range []string{"cross", "mcp", "remote", "cancel"} {
		before := backend.client.calls.Load()
		results, err := runCases(ctx, app.URL, "model-test-token", name, &answers{auto: true, out: io.Discard})
		require.NoError(t, err, "案例 %s", name)
		require.Len(t, results, 1)
		delta := backend.client.calls.Load() - before
		if name == "cancel" {
			require.Zero(t, delta)
		} else {
			require.EqualValues(t, 1, delta)
			var state reviewState
			require.NoError(t, json.Unmarshal(results[0].Result, &state))
			require.Contains(t, state.Result, liveMarker)
			require.NotNil(t, state.Model)
			require.Equal(t, backend.model, state.Model.Model)
			require.Positive(t, state.Model.Usage.TotalTokens)
		}
		evidence = append(evidence, liveCaseEvidence{results[0], delta})
	}
	for _, name := range []string{"cross", "mcp"} {
		before := backend.client.calls.Load()
		results, err := runCases(ctx, app.URL, "model-test-token", name, &answers{scanner: bufio.NewScanner(strings.NewReader("合成审批拒绝说明\nreject\n")), out: io.Discard})
		require.NoError(t, err)
		require.Equal(t, before, backend.client.calls.Load(), "拒绝后不能生成发布摘要")
		var state reviewState
		require.NoError(t, json.Unmarshal(results[0].Result, &state))
		require.Equal(t, "reject", state.Decision)
		require.Nil(t, state.Model)
		result := results[0]
		result.Case += "-reject"
		evidence = append(evidence, liveCaseEvidence{result, 0})
	}
	require.EqualValues(t, 3, backend.client.calls.Load())
	return evidence
}

func TestModelWiringAcrossProtocols(t *testing.T) {
	backend, err := wrapModel(fixtureModel{}, "fixture", "fixture", 3)
	require.NoError(t, err)
	verifyModelProtocols(t, backend)
}

func TestModelRequestLimitAndDurableReplay(t *testing.T) {
	backend, err := wrapModel(fixtureModel{}, "fixture", "fixture", 1)
	require.NoError(t, err)
	journal, err := durable.NewFileStore(t.TempDir())
	require.NoError(t, err)
	ctx := durable.WithJournal(durable.WithExecutionKey(context.Background(), "node-1"), journal)
	input := reviewState{Scenario: "echo", Message: "重放验证"}
	first, err := backend.summarize(ctx, input)
	require.NoError(t, err)
	second, err := backend.summarize(ctx, input)
	require.NoError(t, err)
	require.Equal(t, first.Result, second.Result)
	require.EqualValues(t, 1, backend.client.calls.Load())
	_, err = backend.summarize(durable.WithExecutionKey(ctx, "node-2"), input)
	require.Error(t, err)
	require.EqualValues(t, 1, backend.client.calls.Load())
}

func TestLiveModelProtocols(t *testing.T) {
	if os.Getenv("GOAGENT_PROTOCOL_LIVE") != "1" {
		t.Skip("设置 GOAGENT_PROTOCOL_LIVE=1 才调用真实模型")
	}
	provider := os.Getenv("GOAGENT_PROTOCOL_PROVIDER")
	if provider == "" {
		provider = "deepseek"
	}
	backend, err := newLiveModel(provider, "", 3)
	require.NoError(t, err)
	evidence := verifyModelProtocols(t, backend)
	report := struct {
		Provider   string             `json:"provider"`
		Model      string             `json:"model"`
		Requests   int64              `json:"provider_invocations"`
		VerifiedAt time.Time          `json:"verified_at"`
		Cases      []liveCaseEvidence `json:"cases"`
	}{provider, backend.model, backend.client.calls.Load(), time.Now().UTC(), evidence}
	raw, err := json.Marshal(report)
	require.NoError(t, err)
	t.Log(string(raw))
	if path := os.Getenv("GOAGENT_PROTOCOL_REPORT"); path != "" {
		require.NoError(t, os.WriteFile(path, raw, 0600))
	}
}
