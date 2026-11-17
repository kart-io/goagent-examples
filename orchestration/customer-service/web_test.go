package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kart-io/goagent-examples/orchestration/customer-service/support"
	"github.com/kart-io/goagent/core/durable"
)

type webEnv struct {
	*hostEnv
	srv *httptest.Server
	ws  *webServer
}

func newWebEnv(t *testing.T, mutate func(*HostConfig)) *webEnv {
	t.Helper()
	e := newHostEnv(t, mutate)
	ws := newWebServer(e.host)
	srv := httptest.NewServer(ws.Handler())
	t.Cleanup(func() {
		srv.Close()
		ws.Close(5 * time.Second)
	})
	return &webEnv{hostEnv: e, srv: srv, ws: ws}
}

func (w *webEnv) do(method, path string, body any) (int, []byte) {
	w.t.Helper()
	var rd bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(w.t, err)
		rd = *bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, w.srv.URL+path, &rd)
	require.NoError(w.t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(w.t, err)
	defer func() { _ = resp.Body.Close() }()
	var buf bytes.Buffer
	_, err = buf.ReadFrom(resp.Body)
	require.NoError(w.t, err)
	return resp.StatusCode, buf.Bytes()
}

func (w *webEnv) postJSON(path string, body any) (int, []byte) {
	return w.do(http.MethodPost, path, body)
}
func (w *webEnv) getJSON(path string) (int, []byte) { return w.do(http.MethodGet, path, nil) }

func errCode(t *testing.T, raw []byte) string {
	t.Helper()
	var e struct {
		Error struct{ Code, Message string }
	}
	require.NoError(t, json.Unmarshal(raw, &e), string(raw))
	require.NotEmpty(t, e.Error.Message)
	return e.Error.Code
}

type convResp struct {
	Entries []ConversationEntry
	Pending []struct {
		MessageID string `json:"message_id"`
		Text      string
		Error     string
	}
}

func (w *webEnv) conv(customer string) convResp {
	w.t.Helper()
	code, raw := w.getJSON("/api/customers/" + customer + "/conversation")
	require.Equal(w.t, http.StatusOK, code, string(raw))
	var c convResp
	require.NoError(w.t, json.Unmarshal(raw, &c))
	return c
}

func (w *webEnv) waitConversation(customer string, ok func(convResp) bool) convResp {
	w.t.Helper()
	var last convResp
	require.Eventually(w.t, func() bool { last = w.conv(customer); return ok(last) }, 5*time.Second, 20*time.Millisecond)
	return last
}

func roleIDs(c convResp, role string) []string {
	var ids []string
	for _, en := range c.Entries {
		if en.Role == role {
			ids = append(ids, en.MessageID)
		}
	}
	return ids
}

func hasService(c convResp) bool { return len(roleIDs(c, "service")) > 0 }

func TestWebMessages(t *testing.T) {
	const refund = "订单 A1001 申请退款 30 元"
	t.Run("发消息后看到回复", func(t *testing.T) {
		w := newWebEnv(t, nil)
		code, _ := w.postJSON("/api/customers/c-1001/messages", map[string]string{"message_id": "m1", "text": refund})
		require.Equal(t, http.StatusAccepted, code)
		c := w.waitConversation("c-1001", func(c convResp) bool { return hasService(c) && len(c.Pending) == 0 })
		require.Equal(t, []string{"m1"}, roleIDs(c, "customer"))
	})
	t.Run("重试同一message_id", func(t *testing.T) {
		once := newHostEnv(t, nil)
		once.message("m1", "c-1001", refund)
		want := len(once.sent(support.KindCustomerReply))
		require.Positive(t, want)

		w := newWebEnv(t, nil)
		for range 2 {
			code, _ := w.postJSON("/api/customers/c-1001/messages", map[string]string{"message_id": "m1", "text": refund})
			require.Equal(t, http.StatusAccepted, code)
		}
		c := w.waitConversation("c-1001", func(c convResp) bool { return hasService(c) && len(c.Pending) == 0 })
		require.Equal(t, []string{"m1"}, roleIDs(c, "customer"))
		require.Len(t, w.sent(support.KindCustomerReply), want)
	})
	t.Run("连续两条", func(t *testing.T) {
		w := newWebEnv(t, nil)
		w.ws.mu.Lock() // 占住写锁，让三个任务同时排队，再一起放行
		for _, id := range []string{"m1", "m2", "m3"} {
			code, _ := w.postJSON("/api/customers/c-1001/messages", map[string]string{"message_id": id, "text": refund})
			require.Equal(t, http.StatusAccepted, code)
		}
		w.ws.mu.Unlock()
		c := w.waitConversation("c-1001", func(c convResp) bool { return len(roleIDs(c, "customer")) == 3 && len(c.Pending) == 0 })
		require.Equal(t, []string{"m1", "m2", "m3"}, roleIDs(c, "customer"))
	})
	t.Run("关闭后拒绝写请求", func(t *testing.T) {
		w := newWebEnv(t, nil)
		w.ws.Close(time.Second)
		for _, p := range []string{"/api/customers/c-1001/messages", "/api/sweep", "/api/tickets/tk-1/reply", "/api/tickets/tk-1/answer"} {
			code, raw := w.postJSON(p, map[string]any{"message_id": "m1", "text": refund, "reply_id": "r1", "approver": "fin-1"})
			require.Equal(t, http.StatusServiceUnavailable, code, p)
			require.Equal(t, "internal", errCode(t, raw))
		}
	})
	t.Run("非法输入", func(t *testing.T) {
		w := newWebEnv(t, nil)
		cases := []struct{ name, customer, id, text string }{
			{"客户ID含斜杠", url.PathEscape("a/b"), "m1", "你好"},
			{"客户ID含点点", url.PathEscape("../x"), "m1", "你好"},
			{"客户ID含空格", url.PathEscape("a b"), "m1", "你好"},
			{"text为空", "c-1001", "m1", ""},
			{"text只有空白", "c-1001", "m1", " \t\n"},
			{"text超长", "c-1001", "m1", strings.Repeat("字", 2001)},
			{"message_id为空", "c-1001", "", "你好"},
			{"message_id含斜杠", "c-1001", "a/b", "你好"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				code, raw := w.postJSON("/api/customers/"+tc.customer+"/messages", map[string]string{"message_id": tc.id, "text": tc.text})
				require.Equal(t, http.StatusBadRequest, code, string(raw))
				require.Equal(t, "bad_request", errCode(t, raw))
			})
		}
		t.Run("请求体不是JSON", func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, w.srv.URL+"/api/customers/c-1001/messages", strings.NewReader("{"))
			require.NoError(t, err)
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			_ = resp.Body.Close()
			require.Equal(t, http.StatusBadRequest, resp.StatusCode)
		})
		t.Run("请求体超过64KiB", func(t *testing.T) {
			code, raw := w.postJSON("/api/customers/c-1001/messages", map[string]string{"message_id": "m1", "text": strings.Repeat("a", 70<<10)})
			require.Equal(t, http.StatusBadRequest, code)
			require.Equal(t, "bad_request", errCode(t, raw))
		})
		tickets, err := w.host.Tickets(context.Background(), "")
		require.NoError(t, err)
		require.Empty(t, tickets)
	})
	t.Run("后台出错可见", func(t *testing.T) {
		w := newWebEnv(t, nil)
		w.store.arm(func(s *durable.Snapshot) bool { return s.Plan != "" })
		code, _ := w.postJSON("/api/customers/c-1001/messages", map[string]string{"message_id": "m1", "text": refund})
		require.Equal(t, http.StatusAccepted, code)
		c := w.waitConversation("c-1001", func(c convResp) bool { return len(c.Pending) == 1 && c.Pending[0].Error != "" })
		require.Equal(t, "m1", c.Pending[0].MessageID)
		require.Equal(t, refund, c.Pending[0].Text)
	})
}

func (w *webEnv) ticketJSON(id string) (int, struct {
	Ticket struct{ Status string }
	Turns  []json.RawMessage
	Busy   bool `json:"turns_busy"`
	Answer struct {
		Running bool
		Error   string
	}
}) {
	w.t.Helper()
	code, raw := w.getJSON("/api/tickets/" + id)
	var v struct {
		Ticket struct{ Status string }
		Turns  []json.RawMessage
		Busy   bool `json:"turns_busy"`
		Answer struct {
			Running bool
			Error   string
		}
	}
	if code == http.StatusOK {
		require.NoError(w.t, json.Unmarshal(raw, &v), string(raw))
	}
	return code, v
}

func TestWebDesk(t *testing.T) {
	t.Run("审核批准", func(t *testing.T) {
		w := newWebEnv(t, nil)
		r := w.message("m1", "c-1001", "订单 A1002 申请退款 800 元")
		code, v := w.ticketJSON(r.TicketID)
		require.Equal(t, http.StatusOK, code)
		require.Equal(t, "waiting_review", v.Ticket.Status)
		code, _ = w.postJSON("/api/tickets/"+r.TicketID+"/answer", map[string]any{"approver": "fin-1", "approve": true})
		require.Equal(t, http.StatusAccepted, code)
		require.Eventually(t, func() bool {
			_, v := w.ticketJSON(r.TicketID)
			return v.Ticket.Status != "waiting_review" && !v.Answer.Running
		}, 5*time.Second, 20*time.Millisecond)
	})
	t.Run("审核不存在的工单", func(t *testing.T) {
		w := newWebEnv(t, nil)
		code, raw := w.postJSON("/api/tickets/tk-none/answer", map[string]any{"approver": "fin-1", "approve": true})
		require.Equal(t, http.StatusNotFound, code)
		require.Equal(t, "not_found", errCode(t, raw))
	})
	t.Run("人工回复", func(t *testing.T) {
		w := newWebEnv(t, nil)
		r := w.message("m1", "c-1001", "乱码乱码")
		path := "/api/tickets/" + r.TicketID + "/reply"
		for range 2 {
			code, raw := w.postJSON(path, map[string]string{"reply_id": "r1", "text": "已为您处理"})
			require.Equal(t, http.StatusOK, code, string(raw))
		}
		manual := 0
		for _, d := range w.sent(support.KindCustomerReply) {
			if d.Message.Title == "人工客服" {
				manual++
			}
		}
		require.Equal(t, 1, manual)

		direct := w.message("m2", "c-2002", "订单 A2001 申请退款 30 元")
		code, raw := w.postJSON("/api/tickets/"+direct.TicketID+"/reply", map[string]string{"reply_id": "r1", "text": "x"})
		require.Equal(t, http.StatusConflict, code)
		require.Equal(t, "conflict", errCode(t, raw))

		code, raw = w.postJSON("/api/tickets/tk-none/reply", map[string]string{"reply_id": "r1", "text": "x"})
		require.Equal(t, http.StatusNotFound, code)
		require.Equal(t, "not_found", errCode(t, raw))

		code, raw = w.postJSON(path, map[string]string{"reply_id": "a/b", "text": "x"})
		require.Equal(t, http.StatusBadRequest, code)
		require.Equal(t, "bad_request", errCode(t, raw))
	})
	t.Run("工单列表", func(t *testing.T) {
		w := newWebEnv(t, nil)
		h := w.message("m1", "c-1001", "乱码乱码")
		w.message("m2", "c-2002", "订单 A2001 申请退款 30 元")
		code, raw := w.getJSON("/api/tickets?customer=c-1001")
		require.Equal(t, http.StatusOK, code)
		var got struct{ Tickets []TicketSummary }
		require.NoError(t, json.Unmarshal(raw, &got))
		require.Len(t, got.Tickets, 1)
		require.Equal(t, h.TicketID, got.Tickets[0].TicketID)
		require.Equal(t, "c-1001", got.Tickets[0].CustomerID)
		require.True(t, got.Tickets[0].NeedsHuman)
	})
	t.Run("sweep", func(t *testing.T) {
		w := newWebEnv(t, nil)
		code, raw := w.postJSON("/api/sweep", nil)
		require.Equal(t, http.StatusOK, code)
		var rep SweepReport
		require.NoError(t, json.Unmarshal(raw, &rep))
	})
	t.Run("未开启记忆", func(t *testing.T) {
		w := newWebEnv(t, nil)
		for _, p := range []string{"/api/customers/c-1001/memories", "/api/customers/c-1001/memories/x"} {
			code, raw := w.getJSON(p)
			require.Equal(t, http.StatusBadRequest, code)
			require.Equal(t, "memory_disabled", errCode(t, raw))
		}
	})
	t.Run("工单不存在", func(t *testing.T) {
		w := newWebEnv(t, nil)
		code, raw := w.getJSON("/api/tickets/tk-none")
		require.Equal(t, http.StatusNotFound, code)
		require.Equal(t, "not_found", errCode(t, raw))
	})
	t.Run("运行锁被占用时处理过程读取不阻塞", func(t *testing.T) {
		w := newWebEnv(t, nil)
		r := w.message("m1", "c-1001", "订单 A1002 申请退款 800 元")
		unlock, err := w.store.Lock(context.Background(), w.row(r.TicketID).RunID)
		require.NoError(t, err)
		start := time.Now()
		code, v := w.ticketJSON(r.TicketID)
		require.Equal(t, http.StatusOK, code)
		require.True(t, v.Busy)
		require.Empty(t, v.Turns)
		require.Equal(t, "waiting_review", v.Ticket.Status)
		require.Less(t, time.Since(start), 5*time.Second)
		require.NoError(t, unlock())
		_, v = w.ticketJSON(r.TicketID)
		require.False(t, v.Busy)
	})
}

func TestWebIsolation(t *testing.T) {
	w := newWebEnv(t, nil)
	for customer, id := range map[string]string{"c-1001": "m1", "c-2002": "m2"} {
		code, _ := w.postJSON("/api/customers/"+customer+"/messages", map[string]string{"message_id": id, "text": "订单 A1001 申请退款 30 元"})
		require.Equal(t, http.StatusAccepted, code)
	}
	c := w.waitConversation("c-2002", func(c convResp) bool { return hasService(c) && len(c.Pending) == 0 })
	own, err := w.host.Tickets(context.Background(), "c-2002")
	require.NoError(t, err)
	require.Len(t, own, 1)
	for _, en := range c.Entries {
		require.Equal(t, own[0].TicketID, en.TicketID)
	}
	for _, p := range c.Pending {
		require.NotEqual(t, "m1", p.MessageID)
	}
}
