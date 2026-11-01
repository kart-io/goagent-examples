// flight-booking demonstrates host authorization around a durable GoAgent graph.
// All flights, travelers, payments and tickets are synthetic.
package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/kart-io/goagent-examples/internal/livemodel"
	"github.com/kart-io/goagent/interfaces"
)

type Traveler struct {
	Ref  string `json:"ref"`
	Kind string `json:"kind"`
}
type Request struct {
	From        string     `json:"from"`
	To          string     `json:"to"`
	DepartDate  string     `json:"depart_date"`
	ReturnDate  string     `json:"return_date,omitempty"`
	Cabin       string     `json:"cabin"`
	BudgetCents int64      `json:"budget_cents"`
	Travelers   []Traveler `json:"travelers"`
}
type Leg struct{ Flight, From, To, Departure, Arrival string }
type Quote struct {
	ID         string     `json:"id"`
	Legs       []Leg      `json:"legs"`
	Travelers  []Traveler `json:"travelers"`
	Cabin      string     `json:"cabin"`
	FareCents  int64      `json:"fare_cents"`
	TaxCents   int64      `json:"tax_cents"`
	TotalCents int64      `json:"total_cents"`
	Currency   string     `json:"currency"`
	Baggage    string     `json:"baggage"`
	Rules      string     `json:"rules"`
	ExpiresAt  time.Time  `json:"expires_at"`
}
type Confirmation struct {
	Kind       string     `json:"kind"`
	ID         string     `json:"id"`
	Token      string     `json:"token,omitempty"`
	Owner      string     `json:"owner"`
	Revision   int        `json:"revision"`
	Digest     string     `json:"digest"`
	ExpiresAt  time.Time  `json:"expires_at"`
	ApprovedBy string     `json:"approved_by,omitempty"`
	ApprovedAt *time.Time `json:"approved_at,omitempty"`
}
type EffectCounts struct {
	Holds    int `json:"holds"`
	Payments int `json:"payments"`
	Tickets  int `json:"tickets"`
	Refunds  int `json:"refunds"`
}
type Result struct {
	ReadOnlyTools   int                   `json:"read_only_tools_this_invocation"`
	Simulated       bool                  `json:"simulated"`
	LiveModel       bool                  `json:"live_model"`
	ID              string                `json:"id"`
	Revision        int                   `json:"revision"`
	Status          string                `json:"status"`
	ExecutionStatus string                `json:"execution_status"`
	NextAction      string                `json:"next_action"`
	Message         string                `json:"message,omitempty"`
	Offers          []Quote               `json:"offers,omitempty"`
	FinalQuote      *Quote                `json:"final_quote,omitempty"`
	HoldID          string                `json:"hold_id,omitempty"`
	Confirmation    *Confirmation         `json:"confirmation,omitempty"`
	Payment         string                `json:"payment,omitempty"`
	Ticket          string                `json:"ticket,omitempty"`
	Refund          string                `json:"refund,omitempty"`
	Effects         EffectCounts          `json:"effects"`
	ModelCalls      int                   `json:"model_calls_this_invocation"`
	Usage           interfaces.TokenUsage `json:"task_usage"`
	UsageEvents     int                   `json:"usage_events_this_invocation"`
}
type Command struct {
	Action   string
	ID       string
	Actor    string
	Revision int
	Token    string
	QuoteID  string
	Input    *Request
	Message  string
}
type Market struct {
	PriceDelta int64
	Currency   string
	Rules      string
	SoldOut    bool
}
type Config struct {
	Dir      string
	Now      func() time.Time
	Scenario string
	Market   Market
	Client   *livemodel.Client
	// AfterCommit exposes the real supplier/graph checkpoint gap for fault injection.
	AfterCommit func(stage string)
}
type booking struct {
	ID                string
	Owner             string
	Revision          int
	Request           Request
	Scenario          string
	Status            string
	Message           string
	Offers            []Quote
	Selected          *Quote
	Selection         *Confirmation
	Final             *Confirmation
	HoldID            string
	SelectionAccepted bool
	FinalAccepted     bool
	Payment           string
	Ticket            string
	Refund            string
	Messages          []string
	LiveModel         bool
}

var safeID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

func digest(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func randomToken() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
func strictJSON(data string, dst any) error {
	d := json.NewDecoder(strings.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return fmt.Errorf("invalid structured input")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return fmt.Errorf("expected one JSON object")
	}
	return nil
}
func (c Config) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}
func validateRequest(r Request, now time.Time) (string, bool) {
	if r.From == "" || r.To == "" || r.DepartDate == "" || r.Cabin == "" || len(r.Travelers) == 0 || r.BudgetCents == 0 {
		return "请补充明确的出发/到达机场、日期、舱位、合成旅客和预算。", true
	}
	airports := map[string]bool{"SIN": true, "PVG": true, "NRT": true, "PEK": true}
	if !airports[r.From] || !airports[r.To] || r.From == r.To {
		return "机场需要使用不同的明确机场代码：SIN/PVG/NRT/PEK。", true
	}
	depart, err := time.Parse("2006-01-02", r.DepartDate)
	if err != nil {
		return "请提供 YYYY-MM-DD 格式的明确出发日期。", true
	}
	if depart.Format("2006-01-02") < now.Format("2006-01-02") {
		return "出发日期不能早于当前日期。", false
	}
	if r.ReturnDate != "" {
		ret, err := time.Parse("2006-01-02", r.ReturnDate)
		if err != nil || ret.Before(depart) {
			return "返程日期必须明确且不早于去程。", false
		}
	}
	if r.Cabin != "economy" && r.Cabin != "business" {
		return "仅支持演示 economy/business 舱位。", false
	}
	if r.BudgetCents < 1 || r.BudgetCents > 100000000 || len(r.Travelers) > 9 {
		return "预算或旅客数量超出演示范围。", false
	}
	adults := 0
	seen := map[string]bool{}
	for _, p := range r.Travelers {
		if !strings.HasPrefix(p.Ref, "demo-") || !safeID.MatchString(p.Ref) || seen[p.Ref] || (p.Kind != "adult" && p.Kind != "child") {
			return "仅接受不同的 demo-* 合成成人/儿童旅客引用。", false
		}
		seen[p.Ref] = true
		if p.Kind == "adult" {
			adults++
		}
	}
	if adults == 0 {
		return "演示儿童旅客需要成人同行。", false
	}
	return "", false
}
func makeConfirmation(kind string, o *booking, v any, expires time.Time) (*Confirmation, error) {
	t, err := randomToken()
	if err != nil {
		return nil, err
	}
	return &Confirmation{Kind: kind, ID: o.ID + fmt.Sprintf("/r%d/", o.Revision) + kind, Token: t, Owner: o.Owner, Revision: o.Revision, Digest: digest(v), ExpiresAt: expires}, nil
}
func confirmationMatches(c *Confirmation, o *booking, cmd Command) bool {
	return c != nil && cmd.Token != "" && c.Token == cmd.Token && c.Owner == cmd.Actor && c.Revision == cmd.Revision && o.Owner == cmd.Actor && o.Revision == cmd.Revision
}
func finalBinding(o *booking) any {
	return struct {
		ID, Owner string
		Revision  int
		Request   Request
		Quote     *Quote
		Hold      string
	}{o.ID, o.Owner, o.Revision, o.Request, o.Selected, o.HoldID}
}
