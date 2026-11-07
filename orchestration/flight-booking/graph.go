package main

import (
	"context"
	"errors"
	"fmt"

	agentErrors "github.com/kart-io/goagent/errors"
	"github.com/kart-io/goagent/workflow"
)

type graphState struct {
	Revision int
	Outcome  string
}

func (s *service) graph(ctx context.Context, o *booking) (*workflow.DurableGraph[graphState], error) {
	g := workflow.NewGraph[graphState]()
	add := func(name string, fn func(context.Context) error) error {
		return g.AddNode(name, func(ctx context.Context, st graphState) (graphState, error) {
			if st.Revision != o.Revision {
				return st, fmt.Errorf("obsolete revision")
			}
			err := fn(ctx)
			st.Outcome = o.Status
			return st, err
		})
	}
	if err := add("search", func(ctx context.Context) error {
		o.Offers = s.offers(o, s.c.now())
		if len(o.Offers) == 0 {
			o.Status = "rejected"
			o.Message = "没有满足条件和预算的合成航班；可调整日期、预算或机场后重新查询。"
		} else {
			o.Status = "awaiting_selection"
			var err error
			o.Selection, err = makeConfirmation("selection", o, o.Offers, o.Offers[0].ExpiresAt)
			if err != nil {
				return err
			}
		}
		return s.save(ctx, o)
	}); err != nil {
		return nil, err
	}
	if err := add("choose", func(ctx context.Context) error {
		if !o.SelectionAccepted || o.Selected == nil {
			return fmt.Errorf("selection not authorized")
		}
		return s.save(ctx, o)
	}); err != nil {
		return nil, err
	}
	if err := add("prepare", func(ctx context.Context) error {
		if !s.quoteStillValid(o) {
			o.Status = "requote_required"
			o.Message = "报价、库存或规则已变化，请修改请求或重新报价并完成两次确认。"
			return s.save(ctx, o)
		}
		if err := s.hold(ctx, o); err != nil {
			return err
		}
		if o.Final == nil {
			var err error
			o.Final, err = makeConfirmation("payment_and_ticketing", o, finalBinding(o), o.Selected.ExpiresAt)
			if err != nil {
				return err
			}
		}
		o.Status = "awaiting_final_confirmation"
		return s.save(ctx, o)
	}); err != nil {
		return nil, err
	}
	if err := add("pay", func(ctx context.Context) error {
		receipt, err := s.effect(ctx, o, "payment")
		switch {
		case errors.Is(err, errUnknown):
			o.Status = "payment_pending"
		case errors.Is(err, errQuoteInvalid):
			o.Status = "requote_required"
			o.FinalAccepted = false
			o.Message = "付款前授权或锁座条件已失效，请重新报价。"
			if releaseErr := s.release(ctx, o); releaseErr != nil {
				return releaseErr
			}
			err = nil
		case errors.Is(err, errDeclined):
			o.Status = "rejected"
			o.Message = "模拟支付被明确拒绝。"
			if releaseErr := s.release(ctx, o); releaseErr != nil {
				return releaseErr
			}
			err = nil
		case err != nil:
			return err
		default:
			o.Payment = receipt
			o.Status = "ticketing_pending"
		}
		return errors.Join(err, s.save(ctx, o))
	}); err != nil {
		return nil, err
	}
	if err := add("issue", func(ctx context.Context) error {
		receipt, err := s.effect(ctx, o, "ticket")
		switch {
		case errors.Is(err, errUnknown):
			o.Status = "ticketing_pending"
		case errors.Is(err, errDeclined):
			o.Status = "refund_pending"
			o.Message = "模拟出票明确失败，进入退款补偿。"
			err = nil
		case err != nil:
			return err
		default:
			o.Ticket = receipt
			o.Status = "ticketed"
		}
		return errors.Join(err, s.save(ctx, o))
	}); err != nil {
		return nil, err
	}
	if err := add("settle", func(ctx context.Context) error {
		if o.Status != "refund_pending" {
			return nil
		}
		receipt, err := s.effect(ctx, o, "refund")
		if err == nil {
			o.Refund = receipt
			o.Status = "refunded"
		}
		return errors.Join(err, s.save(ctx, o))
	}); err != nil {
		return nil, err
	}
	for _, e := range [][2]string{{workflow.START, "search"}, {"choose", "prepare"}, {"issue", "settle"}, {"settle", workflow.END}} {
		if err := g.AddEdge(e[0], e[1]); err != nil {
			return nil, err
		}
	}
	for _, b := range []struct{ from, good, want string }{{"search", "choose", "awaiting_selection"}, {"prepare", "pay", "awaiting_final_confirmation"}, {"pay", "issue", "ticketing_pending"}} {
		node := b
		if err := g.AddBranch(node.from, func(_ context.Context, st graphState) (string, error) {
			if st.Outcome == node.want {
				return node.good, nil
			}
			return workflow.END, nil
		}, node.good, workflow.END); err != nil {
			return nil, err
		}
	}
	return g.CompileDurable(ctx, workflow.DurableConfig{Store: s.graphs, Version: "flight-booking", ApprovalNodes: []string{"choose", "pay"}}, workflow.WithBeforeApprove(func(ctx context.Context, _ string, snap *workflow.GraphSnapshot[graphState]) error {
		if snap.State.Revision != o.Revision {
			return fmt.Errorf("obsolete revision")
		}
		switch snap.Node {
		case "choose":
			if !o.SelectionAccepted {
				return fmt.Errorf("selection not authorized")
			}
		case "pay":
			if !o.FinalAccepted || o.Final == nil || o.Final.Digest != digest(finalBinding(o)) {
				return fmt.Errorf("payment not authorized")
			}
		}
		return ctx.Err()
	}))
}
func (s *service) advance(ctx context.Context, o *booking) error {
	g, err := s.graph(ctx, o)
	if err != nil {
		return err
	}
	id := supplierKey(o)
	snap, err := g.Run(ctx, id, graphState{Revision: o.Revision}, "")
	if errors.Is(err, agentErrors.ErrApprovalRequired) {
		key := ""
		if snap.Node == "choose" && o.SelectionAccepted || snap.Node == "pay" && o.FinalAccepted {
			key = snap.Key
		}
		if key != "" {
			_, err = g.Run(ctx, id, graphState{Revision: o.Revision}, key)
		}
	}
	if errors.Is(err, agentErrors.ErrApprovalRequired) || errors.Is(err, errUnknown) {
		return nil
	}
	return err
}
