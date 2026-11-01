package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/kart-io/goagent/core/durable"
)

// Run serializes a whole host command. Graph and supplier stores are independent;
// a supplier commit is not rolled back when a graph checkpoint is not saved.
func Run(ctx context.Context, c Config, cmd Command) (out Result, err error) {
	if c.Dir == "" || !safeID.MatchString(cmd.ID) || cmd.Actor != "demo-traveler" {
		return out, fmt.Errorf("directory, safe request ID and demo-traveler identity required")
	}
	if cmd.Input != nil && cmd.Action != "start" && cmd.Action != "revise" {
		return out, fmt.Errorf("replacement input requires start or revise; never attach it to confirmation")
	}
	if cmd.Message != "" && cmd.Action != "message" {
		return out, fmt.Errorf("message requires message action")
	}
	if cmd.Token != "" && cmd.Action != "select" && cmd.Action != "confirm" {
		return out, fmt.Errorf("confirmation token requires select or confirm action")
	}
	if cmd.QuoteID != "" && cmd.Action != "select" {
		return out, fmt.Errorf("quote ID requires select action")
	}
	if cmd.Action == "" {
		cmd.Action = "status"
	}
	switch cmd.Action {
	case "start", "status", "select", "confirm", "cancel", "revise", "message", "compare":
	default:
		return out, fmt.Errorf("unknown action")
	}
	s := &service{c: c}
	if s.business, err = durable.NewFileStore(filepath.Join(c.Dir, "business")); err != nil {
		return out, err
	}
	if s.supplier, err = durable.NewFileStore(filepath.Join(c.Dir, "supplier")); err != nil {
		return out, err
	}
	if s.graphs, err = durable.NewFileStore(filepath.Join(c.Dir, "graphs")); err != nil {
		return out, err
	}
	unlock, err := s.business.Lock(ctx, cmd.ID)
	if err != nil {
		return out, err
	}
	defer func() { err = errors.Join(err, unlock()) }()
	o, found, err := loadRecord[booking](ctx, s.business, cmd.ID, "flight-booking")
	if err != nil {
		return out, err
	}
	if found && (o.Owner != cmd.Actor || o.ID != cmd.ID) {
		return out, fmt.Errorf("request owner mismatch")
	}
	if !found {
		if cmd.Action != "start" && cmd.Action != "message" {
			return out, fmt.Errorf("request does not exist")
		}
		allowed := map[string]bool{"": true, "normal": true, "no-flights": true, "payment-declined": true, "payment-unknown": true, "ticket-unknown": true, "ticket-failed": true, "refund-unknown": true}
		if !allowed[c.Scenario] {
			return out, fmt.Errorf("unknown synthetic scenario")
		}
		o = booking{ID: cmd.ID, Owner: cmd.Actor, Revision: 1, Scenario: c.Scenario, Status: "needs_input"}
		if cmd.Input != nil {
			o.Request = *cmd.Input
		}
	} else if cmd.Action == "start" && cmd.Input != nil && digest(*cmd.Input) != digest(o.Request) {
		return out, fmt.Errorf("request ID reused with different payload; use revise")
	}
	if cmd.Action == "select" || cmd.Action == "confirm" || cmd.Action == "cancel" || cmd.Action == "revise" {
		if cmd.Revision != o.Revision {
			return out, fmt.Errorf("obsolete revision")
		}
	}
	// Metadata and model usage are collected even for failed Agent decisions.
	calls, events, usage, finish, err := s.usageContext(ctx, &o)
	if err != nil {
		return out, err
	}
	ctx = calls
	defer func() {
		u, e := finish()
		out.Usage = u
		out.UsageEvents = *events
		out.ModelCalls = *usage
		err = errors.Join(err, e)
	}()
	if cmd.Action == "compare" {
		if o.Status != "awaiting_selection" && o.Status != "awaiting_final_confirmation" {
			return out, fmt.Errorf("comparison requires current offers")
		}
		n, e := s.compare(ctx, &o)
		if e != nil {
			return out, e
		}
		if e = s.save(ctx, &o); e != nil {
			return out, e
		}
		out, e = s.result(ctx, &o)
		out.ReadOnlyTools = n
		return out, e
	}
	if cmd.Action == "message" {
		if o.Status != "needs_input" {
			return out, fmt.Errorf("use explicit select/confirm/cancel/revise commands for existing itinerary")
		}
		if err = s.converse(ctx, &o, cmd.Message); err != nil {
			return out, err
		}
	}
	switch cmd.Action {
	case "revise":
		r, e := s.receipt(ctx, &o)
		if e != nil {
			return out, e
		}
		if r.Payment != "" {
			return out, fmt.Errorf("paid booking cannot be revised; active changes require human handling")
		}
		if cmd.Input == nil {
			return out, fmt.Errorf("revise requires complete input")
		}
		if e = s.release(ctx, &o); e != nil {
			return out, e
		}
		o = booking{ID: o.ID, Owner: o.Owner, Revision: o.Revision + 1, Request: *cmd.Input, Scenario: o.Scenario, Status: "needs_input", Messages: o.Messages, LiveModel: o.LiveModel}
	case "cancel":
		r, e := s.receipt(ctx, &o)
		if e != nil {
			return out, e
		}
		if r.Payment != "" {
			o.Status = "needs_attention"
			o.Message = "已有付款，需要人工处理主动退改签；未执行新的取消或退款操作。"
		} else {
			if e = s.release(ctx, &o); e != nil {
				return out, e
			}
			o.Status = "canceled"
			o.Message = "预订已取消，未付款。"
		}
	case "select":
		if !confirmationMatches(o.Selection, &o, cmd) {
			return out, fmt.Errorf("selection confirmation mismatch")
		}
		if o.SelectionAccepted {
			if o.Selected == nil || cmd.QuoteID != o.Selected.ID {
				return out, fmt.Errorf("selection already bound to another quote")
			}
			break
		}
		if o.Status != "awaiting_selection" {
			return out, fmt.Errorf("not waiting for selection")
		}
		if !c.now().Before(o.Selection.ExpiresAt) {
			o.Status = "requote_required"
			break
		}
		for _, q := range o.Offers {
			if q.ID == cmd.QuoteID {
				copy := q
				o.Selected = &copy
			}
		}
		if o.Selected == nil {
			return out, fmt.Errorf("unknown quote")
		}
		if o.Selection.Digest != digest(o.Offers) {
			return out, fmt.Errorf("selection snapshot changed")
		}
		o.SelectionAccepted = true
		now := c.now()
		o.Selection.ApprovedBy = cmd.Actor
		o.Selection.ApprovedAt = &now
	case "confirm":
		if !confirmationMatches(o.Final, &o, cmd) {
			return out, fmt.Errorf("final confirmation mismatch")
		}
		if o.FinalAccepted {
			break
		}
		if o.Status != "awaiting_final_confirmation" {
			return out, fmt.Errorf("not waiting for final confirmation")
		}
		if !c.now().Before(o.Final.ExpiresAt) || !s.quoteStillValid(&o) || o.Final.Digest != digest(finalBinding(&o)) {
			o.Status = "requote_required"
			o.Message = "最终报价、库存、规则或确认有效期已变化；需要重新报价及两次确认。"
			if err = s.release(ctx, &o); err != nil {
				return out, err
			}
			break
		}
		o.FinalAccepted = true
		now := c.now()
		o.Final.ApprovedBy = cmd.Actor
		o.Final.ApprovedAt = &now
	}
	if o.Status == "needs_input" && (cmd.Action == "start" || cmd.Action == "message" || cmd.Action == "revise") {
		msg, missing := validateRequest(o.Request, c.now())
		if msg != "" {
			o.Message = msg
			if !missing {
				o.Status = "rejected"
			}
		} else {
			o.Status = "ready"
			o.Message = ""
		}
	}
	if err = s.save(ctx, &o); err != nil {
		return out, err
	}
	switch o.Status {
	case "ready", "awaiting_selection", "awaiting_final_confirmation", "payment_pending", "ticketing_pending", "refund_pending":
		err = s.advance(ctx, &o)
	}
	if err != nil {
		return out, err
	}
	return s.result(ctx, &o)
}
func (s *service) result(ctx context.Context, o *booking) (Result, error) {
	r, err := s.receipt(ctx, o)
	if err != nil {
		return Result{}, err
	}
	out := Result{Simulated: true, LiveModel: o.LiveModel, ID: o.ID, Revision: o.Revision, Status: o.Status, Message: o.Message, Offers: o.Offers, FinalQuote: o.Selected, HoldID: o.HoldID, Payment: r.Payment, Ticket: r.Ticket, Refund: r.Refund, Effects: r.Effects, ExecutionStatus: "completed"}
	switch o.Status {
	case "needs_input":
		out.NextAction = "message"
		out.ExecutionStatus = "waiting"
	case "awaiting_selection":
		out.NextAction = "select"
		out.Confirmation = o.Selection
		out.ExecutionStatus = "waiting"
	case "awaiting_final_confirmation":
		out.NextAction = "confirm"
		out.Confirmation = o.Final
		out.ExecutionStatus = "waiting"
	case "requote_required":
		out.NextAction = "revise"
	case "payment_pending", "ticketing_pending", "refund_pending":
		out.NextAction = "status"
		out.ExecutionStatus = "pending"
	case "needs_attention":
		out.NextAction = "human_handoff"
	default:
		out.NextAction = "none"
	}
	return out, nil
}
