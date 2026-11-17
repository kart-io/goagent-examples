package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	agentErrors "github.com/kart-io/goagent/errors"
	"github.com/kart-io/goagent/multiagent"

	"github.com/kart-io/goagent-examples/internal/livemodel"
)

const (
	webSubmitter  = "agent-1"
	maxBodyBytes  = 64 << 10
	maxTextRunes  = 2000
	turnsReadWait = time.Second
)

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

type badRequestError string

func (e badRequestError) Error() string { return string(e) }

var errClosing = errors.New("服务正在关闭")

var errMemoryDisabled = errors.New("未开启长期记忆（启动 serve 时加 -memory）")

// apiErrors 按顺序匹配，第一条命中的决定状态码与错误码；未命中为 500 internal。
var apiErrors = []struct {
	status int
	code   string
	match  func(error) bool
}{
	{http.StatusServiceUnavailable, "internal", func(err error) bool { return errors.Is(err, errClosing) }},
	{http.StatusBadRequest, "bad_request", func(err error) bool { var e badRequestError; return errors.As(err, &e) }},
	{http.StatusBadRequest, "memory_disabled", func(err error) bool { return errors.Is(err, errMemoryDisabled) }},
	{http.StatusNotFound, "not_found", func(err error) bool { var e ticketNotFoundError; return errors.As(err, &e) }},
	{http.StatusNotFound, "not_found", func(err error) bool { return agentErrors.IsCode(err, agentErrors.CodeStoreNotFound) }},
	{http.StatusConflict, "conflict", func(err error) bool { return errors.Is(err, errNotHandedOff) }},
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, err error) {
	status, code := http.StatusInternalServerError, "internal"
	for _, c := range apiErrors {
		if c.match(err) {
			status, code = c.status, c.code
			break
		}
	}
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": livemodel.Redact(err.Error())}})
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) error {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(v); err != nil {
		return badRequestError("请求体必须是不超过 64 KiB 的 JSON")
	}
	return nil
}

func checkID(field, v string) error {
	if !idPattern.MatchString(v) {
		return badRequestError(fmt.Sprintf("%s 只能包含字母、数字、下划线与连字符，且不超过 64 个字符", field))
	}
	return nil
}

func checkText(v string) error {
	if n := utf8.RuneCountInString(v); strings.TrimSpace(v) == "" || n > maxTextRunes {
		return badRequestError(fmt.Sprintf("text 不能为空且不超过 %d 个字符", maxTextRunes))
	}
	return nil
}

// jobState 记录一个后台任务；成功且无并行任务时删除，失败保留 err 供页面显示。
type jobState struct {
	customer, text string
	running        int
	err            string
}

// webServer 持有宿主、写锁与后台任务；base 是后台任务的上下文，Close 时在等待超时后取消。
type webServer struct {
	host *Host
	mux  *http.ServeMux

	mu sync.Mutex // 串行化所有写操作；读接口不加锁

	jobsMu sync.Mutex
	jobs   map[string]*jobState
	queue  []queuedJob // 先进先出，由唯一的 worker 按入队顺序执行
	closed bool
	wake   chan struct{}
	done   chan struct{} // worker 退出后关闭
	base   context.Context
	cancel context.CancelFunc
}

type queuedJob struct {
	key string
	st  *jobState
	fn  func(context.Context) error
}

func newWebServer(h *Host) *webServer {
	base, cancel := context.WithCancel(context.Background())
	s := &webServer{host: h, mux: http.NewServeMux(), jobs: map[string]*jobState{}, wake: make(chan struct{}, 1), done: make(chan struct{}), base: base, cancel: cancel}
	for pattern, fn := range map[string]func(http.ResponseWriter, *http.Request) error{
		"POST /api/customers/{c}/messages":     s.postMessage,
		"GET /api/customers/{c}/conversation":  s.getConversation,
		"GET /api/tickets":                     s.listTickets,
		"GET /api/tickets/{t}":                 s.getTicket,
		"POST /api/tickets/{t}/answer":         s.postAnswer,
		"POST /api/tickets/{t}/reply":          s.postReply,
		"POST /api/sweep":                      s.postSweep,
		"GET /api/customers/{c}/memories":      s.getMemories,
		"GET /api/customers/{c}/memories/{id}": s.getMemory,
	} {
		s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if err := fn(w, r); err != nil {
				writeError(w, err)
			}
		})
	}
	go s.run()
	return s
}

func (s *webServer) Handler() http.Handler { return s.mux }

// Close 不再接受新任务，让 worker 在 wait 内排空队列；超时则取消 base，再等 worker 退出。
func (s *webServer) Close(wait time.Duration) {
	s.jobsMu.Lock()
	s.closed = true
	s.jobsMu.Unlock()
	s.signal()
	select {
	case <-s.done:
	case <-time.After(wait):
		s.cancel()
		<-s.done
	}
	s.cancel()
}

func (s *webServer) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *webServer) checkOpen() error {
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	if s.closed {
		return errClosing
	}
	return nil
}

// run 是唯一的 worker：按入队顺序持写锁执行任务，保证先到的消息先写入账本。
func (s *webServer) run() {
	defer close(s.done)
	for {
		s.jobsMu.Lock()
		if len(s.queue) == 0 {
			closed := s.closed
			s.jobsMu.Unlock()
			if closed {
				return
			}
			<-s.wake
			continue
		}
		j := s.queue[0]
		s.queue = s.queue[1:]
		s.jobsMu.Unlock()

		s.mu.Lock()
		err := j.fn(s.base)
		s.mu.Unlock()

		s.jobsMu.Lock()
		j.st.running--
		if err != nil {
			j.st.err = livemodel.Redact(err.Error())
		}
		if j.st.running == 0 && j.st.err == "" {
			delete(s.jobs, j.key)
		}
		s.jobsMu.Unlock()
	}
}

// start 把 fn 排入队列，running 同时统计排队与执行中的任务。skipRunning 为真时，同键任务未完成则不重复入队。
func (s *webServer) start(key string, job jobState, skipRunning bool, fn func(context.Context) error) error {
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	if s.closed {
		return errClosing
	}
	st := s.jobs[key]
	if st == nil {
		st = &jobState{}
		s.jobs[key] = st
	}
	if skipRunning && st.running > 0 {
		return nil
	}
	st.customer, st.text, st.err = job.customer, job.text, ""
	st.running++
	s.queue = append(s.queue, queuedJob{key: key, st: st, fn: fn})
	s.signal()
	return nil
}

func (s *webServer) job(key string) jobState {
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	if st := s.jobs[key]; st != nil {
		return *st
	}
	return jobState{}
}

func (s *webServer) postMessage(w http.ResponseWriter, r *http.Request) error {
	customer := r.PathValue("c")
	var body struct {
		MessageID string `json:"message_id"`
		Text      string `json:"text"`
	}
	if err := checkID("客户 ID", customer); err != nil {
		return err
	}
	if err := decodeBody(w, r, &body); err != nil {
		return err
	}
	if err := checkID("message_id", body.MessageID); err != nil {
		return err
	}
	if err := checkText(body.Text); err != nil {
		return err
	}
	err := s.start("message:"+body.MessageID, jobState{customer: customer, text: body.Text}, true, func(ctx context.Context) error {
		_, err := s.host.Message(ctx, customer, webSubmitter, body.MessageID, body.Text)
		return err
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"message_id": body.MessageID})
	return nil
}

type pendingView struct {
	MessageID string `json:"message_id"`
	Text      string `json:"text"`
	Error     string `json:"error"`
}

func (s *webServer) pending(customer string) []pendingView {
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	out := []pendingView{}
	for key, st := range s.jobs {
		if id, ok := strings.CutPrefix(key, "message:"); ok && st.customer == customer {
			out = append(out, pendingView{MessageID: id, Text: st.text, Error: st.err})
		}
	}
	return out
}

func (s *webServer) getConversation(w http.ResponseWriter, r *http.Request) error {
	customer := r.PathValue("c")
	if err := checkID("客户 ID", customer); err != nil {
		return err
	}
	entries, err := s.host.Conversation(r.Context(), customer)
	if err != nil {
		return err
	}
	if entries == nil {
		entries = []ConversationEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries, "pending": s.pending(customer)})
	return nil
}

func (s *webServer) listTickets(w http.ResponseWriter, r *http.Request) error {
	customer := r.URL.Query().Get("customer")
	if customer != "" {
		if err := checkID("客户 ID", customer); err != nil {
			return err
		}
	}
	tickets, err := s.host.Tickets(r.Context(), customer)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"tickets": tickets})
	return nil
}

func (s *webServer) getTicket(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("t")
	view, err := s.host.Show(r.Context(), id)
	if err != nil {
		return err
	}
	// 后台运行持有运行锁时快照读会一直等到锁超时，页面轮询不能被拖住。
	ctx, cancel := context.WithTimeout(r.Context(), turnsReadWait)
	defer cancel()
	turns, err := s.host.AgentTurns(ctx, id)
	busy := errors.Is(err, context.DeadlineExceeded)
	if busy {
		turns, err = nil, nil
	}
	if err != nil {
		return err
	}
	if turns == nil {
		turns = []multiagent.Turn{}
	}
	st := s.job("answer:" + id)
	writeJSON(w, http.StatusOK, map[string]any{
		"ticket": view, "turns": turns, "turns_busy": busy,
		"answer": map[string]any{"running": st.running > 0, "error": st.err},
	})
	return nil
}

func (s *webServer) postAnswer(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("t")
	var body struct {
		Approver string `json:"approver"`
		Approve  bool   `json:"approve"`
		Note     string `json:"note"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		return err
	}
	if err := checkID("approver", body.Approver); err != nil {
		return err
	}
	if err := s.checkOpen(); err != nil {
		return err
	}
	if _, err := s.host.Show(r.Context(), id); err != nil {
		return err
	}
	err := s.start("answer:"+id, jobState{}, false, func(ctx context.Context) error {
		_, err := s.host.Answer(ctx, id, body.Approver, body.Approve, body.Note)
		return err
	})
	if err != nil {
		return err
	}
	w.WriteHeader(http.StatusAccepted)
	return nil
}

func (s *webServer) postReply(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		ReplyID string `json:"reply_id"`
		Text    string `json:"text"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		return err
	}
	if err := checkID("reply_id", body.ReplyID); err != nil {
		return err
	}
	if err := checkText(body.Text); err != nil {
		return err
	}
	if err := s.checkOpen(); err != nil {
		return err
	}
	s.mu.Lock()
	res, err := s.host.Reply(r.Context(), r.PathValue("t"), body.ReplyID, body.Text)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, res)
	return nil
}

func (s *webServer) postSweep(w http.ResponseWriter, r *http.Request) error {
	if err := s.checkOpen(); err != nil {
		return err
	}
	s.mu.Lock()
	report, err := s.host.Sweep(r.Context())
	s.mu.Unlock()
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, report)
	return nil
}

func (s *webServer) getMemories(w http.ResponseWriter, r *http.Request) error {
	customer := r.PathValue("c")
	if err := s.memoryReady(customer); err != nil {
		return err
	}
	v, err := s.host.Memories(r.Context(), customer, r.URL.Query().Get("q"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, v)
	return nil
}

func (s *webServer) getMemory(w http.ResponseWriter, r *http.Request) error {
	customer := r.PathValue("c")
	if err := s.memoryReady(customer); err != nil {
		return err
	}
	rec, err := s.host.MemoryRecord(r.Context(), customer, r.PathValue("id"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, rec)
	return nil
}

func (s *webServer) memoryReady(customer string) error {
	if err := checkID("客户 ID", customer); err != nil {
		return err
	}
	if s.host.memory == nil {
		return errMemoryDisabled
	}
	return nil
}
