package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kart-io/goagent-examples/internal/livemodel/livemodeltest"
	"github.com/kart-io/goagent/memory"
)

// TestLiveRefundTriage 用真实模型跑一次退款分流，只在 GOAGENT_LIVE=1 时运行，会产生费用；业务仍为模拟。
func TestLiveRefundTriage(t *testing.T) {
	client := livemodeltest.Require(t)
	e := newHostEnv(t, func(c *HostConfig) { c.Client, c.LockTimeout = client, 10*time.Minute })
	before := client.Calls()
	r, err := e.host.Message(context.Background(), "c-1001", "agent-1", "live-1", "订单 A1001 申请退款 30 元")
	evidence, _ := json.Marshal(map[string]any{"provider": client.Live.Provider, "model": client.Live.Model, "status": r.Status,
		"network_calls": client.Calls() - before, "refunds": e.refunds("A1001"), "error": errString(err)})
	t.Log("LIVE_EVIDENCE " + string(evidence))
	require.NoError(t, err)
	require.Contains(t, []string{statusDelivered, statusWaitingReview, statusWaitingCustomer}, r.Status)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// TestLiveMemory 用真实模型与 Gemini 嵌入跑长期记忆的抽取与钉住记录更新，需要 GOAGENT_LIVE=1、GEMINI_API_KEY 与两个存储环境变量，会产生费用。
func TestLiveMemory(t *testing.T) {
	client := livemodeltest.Require(t)
	pg, qd := os.Getenv("CS_MEMORY_PG_URL"), os.Getenv("CS_MEMORY_QDRANT_ADDR")
	if os.Getenv("GEMINI_API_KEY") == "" || pg == "" || qd == "" {
		t.Skip("需要 GEMINI_API_KEY、CS_MEMORY_PG_URL 与 CS_MEMORY_QDRANT_ADDR，请经 scripts/with-stores.sh 运行")
	}
	e := newHostEnv(t, func(c *HostConfig) {
		c.Client, c.LockTimeout = client, 10*time.Minute
		c.Memory = &MemoryConfig{PGURL: pg, QdrantAddr: qd, Live: true}
	})
	c, cctx := newCustomer()
	ctx := context.Background()

	type ticketEvidence struct {
		ID        string `json:"ticket_id"`
		Status    string `json:"final_status"`
		FollowUps int    `json:"follow_ups"`
		Approvals int    `json:"approvals"`
	}
	var tickets []*ticketEvidence
	// 每张工单先走到终态再发下一条，否则新消息会被当作追问回答归进旧工单。
	say := func(text string) {
		r, err := e.host.Message(ctx, c, "agent-1", msgID(), text)
		require.NoError(t, err)
		require.Equal(t, "started", r.Action, "新消息应开启新工单，实际 %s", r.Action)
		ev := &ticketEvidence{ID: r.TicketID}
		tickets = append(tickets, ev)
		for ev.FollowUps+ev.Approvals < 4 {
			switch status := e.row(r.TicketID).Status; status {
			case statusWaitingCustomer:
				require.Less(t, ev.FollowUps, 2, "工单 %s 追问超过 2 次仍未结束", r.TicketID)
				_, err := e.host.Message(ctx, c, "agent-1", msgID(), "没有其他补充，按原支付方式退回即可")
				require.NoError(t, err)
				ev.FollowUps++
			case statusWaitingReview:
				var err error
				for _, approver := range []string{"lead-1", "fin-1"} {
					if _, err = e.host.Answer(ctx, r.TicketID, approver, true, "核实无误"); err == nil {
						break
					}
				}
				require.NoError(t, err)
				ev.Approvals++
			default:
				ev.Status = status
			}
			if ev.Status != "" {
				break
			}
		}
		if ev.Status == "" {
			ev.Status = e.row(r.TicketID).Status
		}
		if ev.Status != statusDone && ev.Status != statusHandoff && ev.Status != statusFailed && ev.Status != statusDelivered {
			t.Fatalf("工单 %s 未到终态：%s", r.TicketID, ev.Status)
		}
	}
	var reports []SweepReport
	sweep := func() {
		r, err := e.host.Sweep(ctx)
		require.NoError(t, err, "扫描无错误")
		reports = append(reports, r)
	}

	say("订单 A1001 申请退款 30 元，退款请只退回原支付方式")
	sweep()
	say("请记住：只用邮件联系我")
	first := pinned(t, e, cctx)
	require.GreaterOrEqual(t, len(first), 1, "memory_save 钉住了客户要求记住的内容")
	say("以后邮件或短信都可以联系我")
	sweep()

	statuses := map[string]memory.JobStatus{}
	for _, tk := range tickets {
		tk := tk.ID
		for _, scope := range []string{refundName, generalName} {
			for _, key := range turnJobKeys(t, e, memorySession(tk, scope)) {
				j, ok, err := e.host.memory.Memory.Job(cctx, key)
				require.NoError(t, err)
				if ok {
					statuses[key] = j.Status
				}
			}
		}
	}
	for key, st := range statuses {
		require.NotEqual(t, memory.JobFailed, st, "抽取任务 %s 不应失败", key)
	}

	after := pinned(t, e, cctx)
	require.NotEmpty(t, after)
	firstIDs := map[string]bool{}
	for _, r := range first {
		firstIDs[r.ID] = true
	}
	updated := false
	for _, r := range after {
		updated = updated || strings.Contains(r.Content, "短信") || firstIDs[r.ID]
	}
	require.True(t, updated, "钉住记录含“短信”，或仍为原钉住记录（模型可能 noop）")

	type pinnedEvidence struct {
		Content      string    `json:"content"`
		SupersededBy string    `json:"superseded_by,omitempty"`
		InvalidAt    time.Time `json:"invalid_at,omitzero"`
	}
	var pins []pinnedEvidence
	for _, r := range first {
		old, err := e.host.memory.Memory.Get(cctx, r.ID)
		require.NoError(t, err)
		pins = append(pins, pinnedEvidence{old.Content, old.SupersededBy, old.InvalidAt})
	}
	for _, r := range after {
		if !firstIDs[r.ID] {
			pins = append(pins, pinnedEvidence{r.Content, r.SupersededBy, r.InvalidAt})
		}
	}
	evidence, _ := json.Marshal(map[string]any{"provider": client.Live.Provider, "model": client.Live.Model,
		"pinned": pins, "tickets": tickets, "job_status": statuses, "sweeps": reports})
	t.Log("LIVE_EVIDENCE " + string(evidence))
}
