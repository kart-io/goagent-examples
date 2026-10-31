// Package scenarios contains executable clients for the HTTP adapter examples.
package scenarios

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/gorilla/websocket"
	"github.com/kart-io/goagent-contrib/httpagent"
)

type Result struct {
	Case   string   `json:"case"`
	RunID  string   `json:"run_id"`
	Status string   `json:"status"`
	Result string   `json:"result,omitempty"`
	Tools  []string `json:"tools,omitempty"`
}

// Run uses the example's /api prefix. Cancellation never automatically resubmits.
func Run(ctx context.Context, client *http.Client, base, kind, input string) (Result, error) {
	result := Result{Case: kind}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.RawQuery != "" || u.Fragment != "" {
		return result, errors.New("url must be an absolute HTTP URL without query or fragment")
	}
	base = strings.TrimRight(base, "/")
	if kind == "ws" || kind == "ws-cancel" {
		return runWS(ctx, base, kind, input)
	}
	path := "/api/invoke"
	payload := map[string]string{"input": input}
	switch kind {
	case "invoke":
	case "sse":
		path = "/api/stream"
	case "webhook":
		path = "/api/webhooks/demo"
		payload = map[string]string{"message": input}
	default:
		return result, fmt.Errorf("unknown case %q", kind)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return result, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(body))
	if err != nil {
		return result, err
	}
	req.Header.Set("Content-Type", "application/json")
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return result, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return result, fmt.Errorf("HTTP %d: %s", resp.StatusCode, b)
	}
	if kind == "sse" {
		return readSSE(resp.Body, result)
	}
	var envelope struct {
		RunID  string            `json:"run_id"`
		Output *httpagent.Output `json:"output"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&envelope); err != nil {
		return result, err
	}
	if envelope.RunID == "" || envelope.Output == nil {
		return result, errors.New("response missing run_id/output")
	}
	result.RunID = envelope.RunID
	result.Status = "completed"
	result.Result = fmt.Sprint(envelope.Output.Result)
	return result, nil
}

func eventResult(result *Result, e httpagent.Event) (bool, error) {
	if e.RunID == "" || (result.RunID != "" && e.RunID != result.RunID) {
		return false, errors.New("event has missing or conflicting run_id")
	}
	result.RunID = e.RunID
	if e.Type == "step" && e.Step == "tool_start" {
		result.Tools = append(result.Tools, e.Tool)
	}
	switch e.Type {
	case "done":
		if e.Output == nil {
			return false, errors.New("done missing output")
		}
		result.Status = "completed"
		result.Result = fmt.Sprint(e.Output.Result)
		return true, nil
	case "error":
		if e.Error == nil {
			return false, errors.New("error terminal missing details")
		}
		if e.Error.Code == "cancelled" {
			result.Status = "cancelled"
			return true, nil
		}
		return false, fmt.Errorf("run failed: %s: %s", e.Error.Code, e.Error.Message)
	}
	return false, nil
}

func readSSE(body io.Reader, result Result) (Result, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 4096), 2<<20)
	var data []string
	terminal := false
	dispatch := func() error {
		if len(data) == 0 {
			return nil
		}
		if terminal {
			return errors.New("business event after terminal")
		}
		var e httpagent.Event
		if err := json.Unmarshal([]byte(strings.Join(data, "\n")), &e); err != nil {
			return err
		}
		var err error
		terminal, err = eventResult(&result, e)
		data = nil
		return err
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if err := dispatch(); err != nil {
				return result, err
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return result, err
	}
	if len(data) > 0 || !terminal {
		return result, errors.New("execution unconfirmed: stream ended without a complete terminal frame")
	}
	return result, nil
}

func runWS(ctx context.Context, base, kind, input string) (Result, error) {
	result := Result{Case: kind}
	target := "ws" + strings.TrimPrefix(base, "http") + "/api/ws"
	dialer := websocket.Dialer{Subprotocols: []string{"goagent.v1"}}
	conn, resp, err := dialer.DialContext(ctx, target, nil)
	if resp != nil && resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	if err != nil {
		return result, err
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	conn.SetReadLimit(2 << 20)
	if deadline, ok := ctx.Deadline(); ok {
		if err = conn.SetReadDeadline(deadline); err != nil {
			return result, err
		}
	}
	if kind == "ws-cancel" {
		input = "慢任务：演示工具开始后取消"
	}
	if err = conn.WriteJSON(map[string]string{"type": "run", "request_id": "example-client", "input": input}); err != nil {
		return result, err
	}
	terminal, cancelled := false, false
	for {
		var frame struct {
			Type  string             `json:"type"`
			RunID string             `json:"run_id"`
			Event *httpagent.Event   `json:"event"`
			Error *httpagent.Failure `json:"error"`
		}
		if err = conn.ReadJSON(&frame); err != nil {
			return result, fmt.Errorf("execution unconfirmed: %w", err)
		}
		switch frame.Type {
		case "accepted":
			if result.RunID != "" || frame.RunID == "" {
				return result, errors.New("invalid accepted frame")
			}
			result.RunID = frame.RunID
		case "event":
			if terminal || result.RunID == "" || frame.RunID != result.RunID || frame.Event == nil {
				return result, errors.New("unexpected event frame")
			}
			terminal, err = eventResult(&result, *frame.Event)
			if err != nil {
				return result, err
			}
			if kind == "ws-cancel" && !cancelled && frame.Event.Step == "tool_start" {
				if err = conn.WriteJSON(map[string]string{"type": "cancel", "run_id": result.RunID}); err != nil {
					return result, err
				}
				cancelled = true
			}
		case "ready":
			if !terminal {
				return result, errors.New("execution unconfirmed: ready before terminal")
			}
			if kind == "ws-cancel" && result.Status != "cancelled" {
				return result, errors.New("cancellation was not confirmed")
			}
			return result, nil
		case "error":
			return result, fmt.Errorf("protocol error: %+v", frame.Error)
		default:
			return result, fmt.Errorf("unexpected frame %q", frame.Type)
		}
	}
}
