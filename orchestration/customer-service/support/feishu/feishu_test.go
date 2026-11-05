package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kart-io/goagent-examples/orchestration/customer-service/support"
)

// fakeOpen 是开放平台替身：记录请求，按路径返回预置响应。
type fakeOpen struct {
	mu       sync.Mutex
	tokens   int
	invalid  int // 剩余几次请求返回令牌无效
	requests []*http.Request
	bodies   []map[string]any
	users    map[string]map[string]any
	pages    map[string]map[string]any // page_token → 成员页
}

func (f *fakeOpen) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.requests = append(f.requests, r)
		f.bodies = append(f.bodies, body)
		reply := func(v any) { require.NoError(t, json.NewEncoder(w).Encode(v)) }
		if r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
			f.tokens++
			require.Equal(t, "app", body["app_id"])
			require.Equal(t, "secret", body["app_secret"])
			reply(map[string]any{"code": 0, "msg": "ok", "tenant_access_token": "t-" + string(rune('0'+f.tokens)), "expire": 7200})
			return
		}
		if f.invalid > 0 {
			f.invalid--
			reply(map[string]any{"code": 99991663, "msg": "invalid access token"})
			return
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/open-apis/contact/v3/users/"):
			id := strings.TrimPrefix(r.URL.Path, "/open-apis/contact/v3/users/")
			u, ok := f.users[id]
			if !ok {
				reply(map[string]any{"code": 41050, "msg": "no user authority"})
				return
			}
			reply(map[string]any{"code": 0, "data": map[string]any{"user": u}})
		case strings.HasSuffix(r.URL.Path, "/member/simplelist"):
			reply(map[string]any{"code": 0, "data": f.pages[r.URL.Query().Get("page_token")]})
		case r.URL.Path == "/open-apis/im/v1/messages":
			reply(map[string]any{"code": 0, "data": map[string]any{"message_id": "om_1"}})
		default:
			w.WriteHeader(http.StatusNotFound)
			reply(map[string]any{"code": 404, "msg": "not found"})
		}
	})
}

func newFake(t *testing.T) (*fakeOpen, *Client, *support.ManualClock) {
	t.Helper()
	f := &fakeOpen{users: map[string]map[string]any{
		"ou_agent": {"open_id": "ou_agent", "name": "客服甲", "leader_user_id": "ou_lead"},
		"ou_lead":  {"open_id": "ou_lead", "name": "组长", "leader_user_id": ""},
		"ou_fin1":  {"open_id": "ou_fin1", "name": "财务一"},
		"ou_fin2":  {"open_id": "ou_fin2", "name": "财务二"},
	}}
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	clock := support.NewManualClock(time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC))
	return f, &Client{BaseURL: srv.URL, AppID: "app", AppSecret: "secret", HTTP: srv.Client(), Now: clock.Now}, clock
}

func (f *fakeOpen) auth(i int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[i].Header.Get("Authorization")
}

func TestTokenCachedAndRefreshed(t *testing.T) {
	f, c, clock := newFake(t)
	ctx := context.Background()
	_, _, err := c.Leader(ctx, "ou_lead")
	require.NoError(t, err)
	_, _, err = c.Leader(ctx, "ou_lead")
	require.NoError(t, err)
	require.Equal(t, 1, f.tokens, "有效期内复用缓存的令牌")
	require.Equal(t, "Bearer t-1", f.auth(2))

	clock.Advance(2*time.Hour - 4*time.Minute)
	_, _, err = c.Leader(ctx, "ou_lead")
	require.NoError(t, err)
	require.Equal(t, 2, f.tokens, "过期前 5 分钟内提前刷新")
	require.Equal(t, "Bearer t-2", f.auth(4))
}

func TestTokenInvalidRetriesOnce(t *testing.T) {
	f, c, _ := newFake(t)
	ctx := context.Background()
	f.invalid = 1
	_, _, err := c.Leader(ctx, "ou_lead")
	require.NoError(t, err, "令牌无效时清除缓存并重试一次")
	require.Equal(t, 2, f.tokens)

	f.invalid = 2
	_, _, err = c.Leader(ctx, "ou_lead")
	require.ErrorIs(t, err, ErrTokenInvalid, "重试后仍无效则返回错误")
	var apiErr *APIError
	require.True(t, errors.As(err, &apiErr))
	require.Equal(t, 99991663, apiErr.Code)
}

func TestLeader(t *testing.T) {
	_, c, _ := newFake(t)
	ctx := context.Background()
	leader, found, err := c.Leader(ctx, "ou_agent")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, support.User{ID: "ou_lead", Name: "组长"}, leader)

	_, found, err = c.Leader(ctx, "ou_lead")
	require.NoError(t, err)
	require.False(t, found, "leader_user_id 为空表示没有上级")

	_, _, err = c.Leader(ctx, "ou_unknown")
	var apiErr *APIError
	require.True(t, errors.As(err, &apiErr), "通讯录权限不足时返回错误码")
	require.Equal(t, 41050, apiErr.Code)
	require.NotErrorIs(t, err, ErrTokenInvalid)
}

func TestGroupMembersPagesAndSkipsDepartments(t *testing.T) {
	f, c, _ := newFake(t)
	f.pages = map[string]map[string]any{
		"": {"memberlist": []map[string]any{
			{"member_id": "ou_fin1", "member_type": "user"},
			{"member_id": "od_dept", "member_type": "department"},
		}, "has_more": true, "page_token": "p2"},
		"p2": {"memberlist": []map[string]any{{"member_id": "ou_fin2", "member_type": "user"}}, "has_more": false},
	}
	var skipped []string
	c.Skipped = func(groupID, memberID, memberType string) {
		skipped = append(skipped, groupID+"/"+memberID+"/"+memberType)
	}
	members, err := c.GroupMembers(context.Background(), "finance-reviewers")
	require.NoError(t, err)
	require.Equal(t, []support.User{{ID: "ou_fin1", Name: "财务一"}, {ID: "ou_fin2", Name: "财务二"}}, members)
	require.Equal(t, []string{"finance-reviewers/od_dept/department"}, skipped)
}

func TestNotifySendsUUIDAndReceiveIDType(t *testing.T) {
	f, c, _ := newFake(t)
	err := c.Notify(context.Background(), support.User{ID: "ou_lead"}, support.Message{Kind: support.KindApprovalRequest, IdempotencyKey: "review/abc", Title: "退款审核", Body: "订单 A1002"})
	require.NoError(t, err)
	last := len(f.requests) - 1
	require.Equal(t, "/open-apis/im/v1/messages", f.requests[last].URL.Path)
	require.Equal(t, "open_id", f.requests[last].URL.Query().Get("receive_id_type"))
	body := f.bodies[last]
	require.Equal(t, "ou_lead", body["receive_id"])
	require.Equal(t, "text", body["msg_type"])
	require.Equal(t, "review/abc", body["uuid"])
	require.JSONEq(t, `{"text":"退款审核\n订单 A1002"}`, body["content"].(string))

	long := strings.Repeat("k", 80)
	require.Len(t, MessageUUID(long), 50, "超过 50 字符的键按摘要截断")
	require.Equal(t, MessageUUID(long), MessageUUID(long))
	require.NotEqual(t, MessageUUID(long), MessageUUID(long+"x"))
}

func TestFromEnv(t *testing.T) {
	t.Setenv("FEISHU_APP_ID", "")
	c, err := FromEnv()
	require.NoError(t, err)
	require.Nil(t, c, "未设置应用 ID 时不开启飞书")

	t.Setenv("FEISHU_APP_ID", "app")
	t.Setenv("FEISHU_APP_SECRET", "")
	_, err = FromEnv()
	require.Error(t, err)

	t.Setenv("FEISHU_APP_SECRET", "secret")
	t.Setenv("FEISHU_BASE_URL", "https://open.example.com/")
	c, err = FromEnv()
	require.NoError(t, err)
	require.Equal(t, "https://open.example.com", c.base())
}
