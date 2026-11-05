// Package feishu 用 net/http 直接调用飞书开放平台：通讯录实现 support.Directory，消息实现 support.Notifier。
// 消息发送只经 httptest 验证，案例不接真实发送；请求与响应字段以开放平台文档为准。
package feishu

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/kart-io/goagent-examples/orchestration/customer-service/support"
)

// DefaultBaseURL 是飞书开放平台地址。
const DefaultBaseURL = "https://open.feishu.cn"

// 令牌在过期前这段时间内视为需要刷新。
const refreshBefore = 5 * time.Minute

// uuid 的最大长度；更长的发件箱键按摘要截断。
const maxUUIDLen = 50

// ErrTokenInvalid 标识令牌无效或缺失，客户端会清除缓存并重试一次。
var ErrTokenInvalid = errors.New("飞书访问令牌无效")

// tokenInvalidCodes 是令牌无效或缺失的错误码。
var tokenInvalidCodes = map[int]bool{99991661: true, 99991663: true}

// APIError 是开放平台返回的非零错误码。
type APIError struct {
	Code int
	Msg  string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("飞书接口返回错误 %d：%s", e.Code, e.Msg)
}

func (e *APIError) Is(target error) bool {
	return target == ErrTokenInvalid && tokenInvalidCodes[e.Code]
}

// Client 是飞书开放平台客户端，并发安全。
type Client struct {
	BaseURL, AppID, AppSecret string
	HTTP                      *http.Client
	Now                       func() time.Time
	ReceiveIDType             string                                     // 默认 open_id
	Skipped                   func(groupID, memberID, memberType string) // 可选：记录被忽略的非用户成员

	mu      sync.Mutex
	token   string
	expires time.Time
}

// FromEnv 读取 FEISHU_APP_ID、FEISHU_APP_SECRET 与可选的 FEISHU_BASE_URL；未设置应用 ID 时返回 nil。
func FromEnv() (*Client, error) {
	id, secret := os.Getenv("FEISHU_APP_ID"), os.Getenv("FEISHU_APP_SECRET")
	if id == "" {
		return nil, nil
	}
	if secret == "" {
		return nil, errors.New("设置了 FEISHU_APP_ID 但缺少 FEISHU_APP_SECRET")
	}
	return &Client{BaseURL: os.Getenv("FEISHU_BASE_URL"), AppID: id, AppSecret: secret}, nil
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) base() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return DefaultBaseURL
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}

// envelope 是开放平台响应的公共字段。
type envelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// send 发出一次请求并解析公共字段；code 非零时返回 *APIError。
func (c *Client) send(ctx context.Context, method, path string, query url.Values, token string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	u := c.base() + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("请求飞书 %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("飞书 %s 返回 HTTP %d，响应无法解析", path, resp.StatusCode)
	}
	if env.Code != 0 {
		return &APIError{Code: env.Code, Msg: env.Msg}
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("飞书 %s 返回 HTTP %d", path, resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	if len(env.Data) > 0 {
		return json.Unmarshal(env.Data, out)
	}
	return json.Unmarshal(data, out)
}

// accessToken 返回缓存的租户令牌，过期前 refreshBefore 内重新获取。
func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && c.now().Before(c.expires.Add(-refreshBefore)) {
		return c.token, nil
	}
	var out struct {
		Token  string `json:"tenant_access_token"`
		Expire int    `json:"expire"`
	}
	body := map[string]string{"app_id": c.AppID, "app_secret": c.AppSecret}
	if err := c.send(ctx, http.MethodPost, "/open-apis/auth/v3/tenant_access_token/internal", nil, "", body, &out); err != nil {
		return "", fmt.Errorf("获取飞书租户令牌: %w", err)
	}
	if out.Token == "" || out.Expire <= 0 {
		return "", errors.New("飞书租户令牌响应缺少令牌或有效期")
	}
	c.token, c.expires = out.Token, c.now().Add(time.Duration(out.Expire)*time.Second)
	return c.token, nil
}

func (c *Client) dropToken() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token = ""
}

// call 带令牌调用接口；令牌无效时清除缓存并重试一次。
func (c *Client) call(ctx context.Context, method, path string, query url.Values, body, out any) error {
	for attempt := 0; ; attempt++ {
		token, err := c.accessToken(ctx)
		if err != nil {
			return err
		}
		err = c.send(ctx, method, path, query, token, body, out)
		if attempt == 0 && errors.Is(err, ErrTokenInvalid) {
			c.dropToken()
			continue
		}
		return err
	}
}

type contactUser struct {
	OpenID       string `json:"open_id"`
	Name         string `json:"name"`
	LeaderUserID string `json:"leader_user_id"`
}

func (c *Client) user(ctx context.Context, id string) (contactUser, error) {
	var out struct {
		User contactUser `json:"user"`
	}
	q := url.Values{"user_id_type": {"open_id"}}
	err := c.call(ctx, http.MethodGet, "/open-apis/contact/v3/users/"+url.PathEscape(id), q, nil, &out)
	return out.User, err
}

// Leader 读取 leader_user_id；为空表示没有上级。需要字段权限 contact:user.department:readonly。
func (c *Client) Leader(ctx context.Context, userID string) (support.User, bool, error) {
	u, err := c.user(ctx, userID)
	if err != nil {
		return support.User{}, false, err
	}
	if u.LeaderUserID == "" {
		return support.User{}, false, nil
	}
	leader, err := c.user(ctx, u.LeaderUserID)
	if err != nil {
		return support.User{}, false, err
	}
	return support.User{ID: u.LeaderUserID, Name: leader.Name}, true, nil
}

// GroupMembers 分页读取用户组成员，只保留用户成员并另行查询用户名；部门等其他成员忽略并经 Skipped 记录。
func (c *Client) GroupMembers(ctx context.Context, groupID string) ([]support.User, error) {
	var users []support.User
	pageToken := ""
	for {
		q := url.Values{"member_id_type": {"open_id"}, "page_size": {"100"}}
		if pageToken != "" {
			q.Set("page_token", pageToken)
		}
		var page struct {
			Members []struct {
				MemberID   string `json:"member_id"`
				MemberType string `json:"member_type"`
			} `json:"memberlist"`
			PageToken string `json:"page_token"`
			HasMore   bool   `json:"has_more"`
		}
		if err := c.call(ctx, http.MethodGet, "/open-apis/contact/v3/group/"+url.PathEscape(groupID)+"/member/simplelist", q, nil, &page); err != nil {
			return nil, err
		}
		for _, m := range page.Members {
			if m.MemberType != "user" {
				if c.Skipped != nil {
					c.Skipped(groupID, m.MemberID, m.MemberType)
				}
				continue
			}
			u, err := c.user(ctx, m.MemberID)
			if err != nil {
				return nil, err
			}
			users = append(users, support.User{ID: m.MemberID, Name: u.Name})
		}
		if !page.HasMore || page.PageToken == "" {
			return users, nil
		}
		pageToken = page.PageToken
	}
}

// MessageUUID 把发件箱键映射为消息接口的 uuid：不超过 50 字符时原样使用，否则取摘要。
// uuid 去重只在约 1 小时内有效，发件箱以本地状态为准。
func MessageUUID(key string) string {
	if len(key) <= maxUUIDLen {
		return key
	}
	sum := sha256.Sum256([]byte(key))
	return "h-" + hex.EncodeToString(sum[:])[:maxUUIDLen-2]
}

// Notify 以文本消息发送；应用需开启机器人，接收人须在应用可用范围内。
func (c *Client) Notify(ctx context.Context, to support.User, m support.Message) error {
	idType := c.ReceiveIDType
	if idType == "" {
		idType = "open_id"
	}
	text := m.Title
	if m.Body != "" {
		text += "\n" + m.Body
	}
	content, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return err
	}
	body := map[string]string{"receive_id": to.ID, "msg_type": "text", "content": string(content), "uuid": MessageUUID(m.IdempotencyKey)}
	return c.call(ctx, http.MethodPost, "/open-apis/im/v1/messages", url.Values{"receive_id_type": {idType}}, body, nil)
}
