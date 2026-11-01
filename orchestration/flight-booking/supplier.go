package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/kart-io/goagent/core/durable"
)

var errUnknown = errors.New("supplier result unknown; reconcile original business key")
var errDeclined = errors.New("synthetic payment declined")
var errQuoteInvalid = errors.New("held conditions or authorization expired")

type supplierRecord struct {
	RequestDigest string
	HoldDigest    string
	HoldReleased  bool
	Payment       string
	Ticket        string
	Refund        string
	Effects       EffectCounts
}
type service struct {
	c        Config
	business durable.Store
	supplier durable.Store
	graphs   durable.Store
}

func loadRecord[T any](ctx context.Context, store durable.Store, id, version string) (v T, found bool, err error) {
	snap, err := store.Load(ctx, id)
	if errors.Is(err, os.ErrNotExist) {
		return v, false, nil
	}
	if err != nil {
		return v, false, err
	}
	if snap.RunID != id || snap.Version != version {
		return v, false, fmt.Errorf("invalid stored record version or ID")
	}
	err = json.Unmarshal(snap.Data, &v)
	return v, true, err
}
func saveRecord(ctx context.Context, store durable.Store, id, version string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return store.Save(ctx, id, &durable.Snapshot{RunID: id, Version: version, Data: data})
}
func (s *service) save(ctx context.Context, o *booking) error {
	return saveRecord(ctx, s.business, o.ID, "flight-booking", o)
}
func supplierKey(o *booking) string { return fmt.Sprintf("%s-r%d", o.ID, o.Revision) }
func (s *service) receipt(ctx context.Context, o *booking) (supplierRecord, error) {
	r, _, err := loadRecord[supplierRecord](ctx, s.supplier, supplierKey(o), "flight-supplier")
	if err != nil {
		return r, err
	}
	if r.RequestDigest != "" && r.RequestDigest != digest(o.Request) {
		return r, fmt.Errorf("supplier business key payload conflict")
	}
	r.RequestDigest = digest(o.Request)
	return r, nil
}
func (s *service) saveReceipt(ctx context.Context, o *booking, r supplierRecord) error {
	return saveRecord(ctx, s.supplier, supplierKey(o), "flight-supplier", r)
}
func (s *service) offers(o *booking, now time.Time) []Quote {
	if o.Scenario == "no-flights" || s.c.Market.SoldOut {
		return nil
	}
	r := o.Request
	fare := int64(0)
	for _, p := range r.Travelers {
		if p.Kind == "adult" {
			fare += 100000
		} else {
			fare += 70000
		}
	}
	if r.Cabin == "business" {
		fare *= 2
	}
	tax := int64(len(r.Travelers)) * 15000
	legs := 1
	if r.ReturnDate != "" {
		legs = 2
	}
	fare *= int64(legs)
	tax *= int64(legs)
	currency := s.c.Market.Currency
	if currency == "" {
		currency = "CNY"
	}
	rules := s.c.Market.Rules
	if rules == "" {
		rules = "demo-rules-1: change=30000/refund=50000 cents; synthetic only"
	}
	zone := func(airport string) string {
		if airport == "NRT" {
			return "+09:00"
		}
		return "+08:00"
	}
	var quotes []Quote
	for i := 0; i < 2; i++ {
		price := fare + tax + s.c.Market.PriceDelta + int64(i)*20000
		if price < 1 || price > r.BudgetCents {
			continue
		}
		q := Quote{ID: fmt.Sprintf("DEMO-%d-r%d", i+1, o.Revision), Cabin: r.Cabin, Travelers: append([]Traveler(nil), r.Travelers...), FareCents: price - tax, TaxCents: tax, TotalCents: price, Currency: currency, Rules: rules, Baggage: "demo: 1 checked bag, 20kg per traveler", ExpiresAt: now.Add(10 * time.Minute)}
		q.Legs = []Leg{{Flight: fmt.Sprintf("DEMO-%d", 101+i), From: r.From, To: r.To, Departure: r.DepartDate + "T10:00:00" + zone(r.From), Arrival: r.DepartDate + "T16:00:00" + zone(r.To)}}
		if legs == 2 {
			q.Legs = append(q.Legs, Leg{Flight: fmt.Sprintf("DEMO-%d", 201+i), From: r.To, To: r.From, Departure: r.ReturnDate + "T18:00:00" + zone(r.To), Arrival: r.ReturnDate + "T23:00:00" + zone(r.From)})
		}
		quotes = append(quotes, q)
	}
	return quotes
}
func (s *service) quoteStillValid(o *booking) bool {
	if o.Selected == nil || !s.c.now().Before(o.Selected.ExpiresAt) {
		return false
	}
	for _, q := range s.offers(o, s.c.now()) {
		if q.ID == o.Selected.ID {
			q.ExpiresAt = o.Selected.ExpiresAt
			return digest(q) == digest(o.Selected)
		}
	}
	return false
}
func (s *service) hold(ctx context.Context, o *booking) error {
	r, err := s.receipt(ctx, o)
	if err != nil {
		return err
	}
	binding := digest(o.Selected)
	if r.HoldDigest != "" {
		if r.HoldDigest != binding || r.HoldReleased {
			return fmt.Errorf("hold no longer valid")
		}
	} else {
		r.HoldDigest = binding
		r.Effects.Holds++
		if err = s.saveReceipt(ctx, o, r); err != nil {
			return err
		}
	}
	o.HoldID = "HOLD-" + supplierKey(o)
	return nil
}
func (s *service) release(ctx context.Context, o *booking) error {
	r, err := s.receipt(ctx, o)
	if err != nil {
		return err
	}
	if r.Payment != "" {
		return fmt.Errorf("cannot release a paid booking")
	}
	if r.HoldDigest != "" {
		r.HoldReleased = true
		return s.saveReceipt(ctx, o, r)
	}
	return nil
}
func (s *service) effect(ctx context.Context, o *booking, kind string) (string, error) {
	r, err := s.receipt(ctx, o)
	if err != nil {
		return "", err
	}
	var value *string
	switch kind {
	case "payment":
		value = &r.Payment
	case "ticket":
		value = &r.Ticket
	case "refund":
		value = &r.Refund
	default:
		return "", fmt.Errorf("unknown effect")
	}
	// A committed result wins over later expiry, cancellation and injected errors.
	if *value != "" {
		return *value, nil
	}
	switch kind {
	case "payment":
		if !o.FinalAccepted || o.Final == nil || o.Final.Digest != digest(finalBinding(o)) || !s.c.now().Before(o.Final.ExpiresAt) || !s.quoteStillValid(o) || r.HoldReleased || r.HoldDigest != digest(o.Selected) {
			return "", errQuoteInvalid
		}
		if o.Scenario == "payment-declined" {
			return "", errDeclined
		}
		r.Effects.Payments++
	case "ticket":
		if r.Payment == "" {
			return "", fmt.Errorf("payment required")
		}
		if !s.quoteStillValid(o) || r.HoldReleased || r.HoldDigest != digest(o.Selected) {
			return "", errDeclined
		}
		if o.Scenario == "ticket-failed" || o.Scenario == "refund-unknown" {
			return "", errDeclined
		}
		r.Effects.Tickets++
	case "refund":
		if r.Payment == "" || r.Ticket != "" {
			return "", fmt.Errorf("only failed paid issuance can be compensated")
		}
		r.Effects.Refunds++
	}
	*value = kind + "-" + supplierKey(o)
	receipt := *value
	if err = s.saveReceipt(ctx, o, r); err != nil {
		return "", err
	}
	if s.c.AfterCommit != nil {
		s.c.AfterCommit(kind)
	}
	if o.Scenario == kind+"-unknown" {
		return "", errUnknown
	}
	return receipt, nil
}
