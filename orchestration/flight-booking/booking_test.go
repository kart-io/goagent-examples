package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func demoRequest() Request {
	return Request{From: "SIN", To: "PVG", DepartDate: "2030-01-10", Cabin: "economy", BudgetCents: 500000, Travelers: []Traveler{{Ref: "demo-adult-1", Kind: "adult"}}}
}
func testConfig(t *testing.T) Config {
	return Config{Dir: t.TempDir(), Now: func() time.Time { return time.Date(2029, 1, 1, 0, 0, 0, 0, time.UTC) }}
}
func command(action string) Command {
	return Command{Action: action, ID: "booking-1", Actor: "demo-traveler", Revision: 1}
}
func begin(t *testing.T, c Config) Result {
	t.Helper()
	cmd := command("start")
	r := demoRequest()
	cmd.Input = &r
	out, err := Run(context.Background(), c, cmd)
	require.NoError(t, err)
	return out
}
func selectOffer(t *testing.T, c Config, r Result) Result {
	t.Helper()
	cmd := command("select")
	cmd.Revision = r.Revision
	cmd.Token = r.Confirmation.Token
	cmd.QuoteID = r.Offers[0].ID
	out, err := Run(context.Background(), c, cmd)
	require.NoError(t, err)
	return out
}
func confirm(t *testing.T, c Config, r Result) Result {
	t.Helper()
	cmd := command("confirm")
	cmd.Revision = r.Revision
	cmd.Token = r.Confirmation.Token
	out, err := Run(context.Background(), c, cmd)
	require.NoError(t, err)
	return out
}

func TestTwoConfirmationsRequired(t *testing.T) {
	c := testConfig(t)
	r := begin(t, c)
	require.Equal(t, "awaiting_selection", r.Status)
	require.Zero(t, r.Effects.Payments)
	final := selectOffer(t, c, r)
	require.Equal(t, "awaiting_final_confirmation", final.Status)
	require.Zero(t, final.Effects.Payments)
	require.NotEqual(t, r.Confirmation.Token, final.Confirmation.Token)
	done := confirm(t, c, final)
	require.Equal(t, "ticketed", done.Status)
	require.Equal(t, 1, done.Effects.Payments)
	require.Equal(t, 1, done.Effects.Tickets)
	require.NotEmpty(t, done.Ticket)
	again := confirm(t, c, final)
	require.Equal(t, done.Ticket, again.Ticket)
	require.Equal(t, done.Effects, again.Effects)
}
func TestConfirmationBoundToOwnerRevisionAndToken(t *testing.T) {
	for _, field := range []string{"actor", "revision", "token"} {
		t.Run(field, func(t *testing.T) {
			c := testConfig(t)
			r := selectOffer(t, c, begin(t, c))
			cmd := command("confirm")
			cmd.Token = r.Confirmation.Token
			switch field {
			case "actor":
				cmd.Actor = "intruder"
			case "revision":
				cmd.Revision = 2
			case "token":
				cmd.Token = "wrong"
			}
			_, err := Run(context.Background(), c, cmd)
			require.Error(t, err)
			out, err := Run(context.Background(), c, command("status"))
			require.NoError(t, err)
			require.Zero(t, out.Effects.Payments)
		})
	}
}
func TestRepriceAndExpiryInvalidateAuthorization(t *testing.T) {
	for _, change := range []string{"price", "currency", "rules", "sold-out", "expiry"} {
		t.Run(change, func(t *testing.T) {
			c := testConfig(t)
			r := selectOffer(t, c, begin(t, c))
			switch change {
			case "price":
				c.Market.PriceDelta = 100
			case "currency":
				c.Market.Currency = "USD"
			case "rules":
				c.Market.Rules = "demo-rules-2"
			case "sold-out":
				c.Market.SoldOut = true
			case "expiry":
				c.Now = func() time.Time { return time.Date(2029, 1, 1, 1, 0, 0, 0, time.UTC) }
			}
			cmd := command("confirm")
			cmd.Token = r.Confirmation.Token
			out, err := Run(context.Background(), c, cmd)
			require.NoError(t, err)
			require.Equal(t, "requote_required", out.Status)
			require.Zero(t, out.Effects.Payments)
		})
	}
}
func TestRevisionCancellationAndPayloadConflict(t *testing.T) {
	c := testConfig(t)
	r := selectOffer(t, c, begin(t, c))
	cmd := command("start")
	req := demoRequest()
	req.To = "NRT"
	cmd.Input = &req
	_, err := Run(context.Background(), c, cmd)
	require.Error(t, err)
	cmd = command("revise")
	cmd.Input = &req
	next, err := Run(context.Background(), c, cmd)
	require.NoError(t, err)
	require.Equal(t, 2, next.Revision)
	require.Equal(t, "awaiting_selection", next.Status)
	old := command("confirm")
	old.Token = r.Confirmation.Token
	_, err = Run(context.Background(), c, old)
	require.Error(t, err)
	cmd = command("cancel")
	cmd.Revision = 2
	out, err := Run(context.Background(), c, cmd)
	require.NoError(t, err)
	require.Equal(t, "canceled", out.Status)
	require.Zero(t, out.Effects.Payments)
}
func TestSupplierFailuresReconcileWithoutDuplicateEffects(t *testing.T) {
	for _, fault := range []string{"payment-declined", "payment-unknown", "ticket-unknown", "ticket-failed", "refund-unknown"} {
		t.Run(fault, func(t *testing.T) {
			c := testConfig(t)
			c.Scenario = fault
			r := selectOffer(t, c, begin(t, c))
			r = confirm(t, c, r)
			for i := 0; i < 3 && (r.Status == "payment_pending" || r.Status == "ticketing_pending" || r.Status == "refund_pending"); i++ {
				var err error
				r, err = Run(context.Background(), c, command("status"))
				require.NoError(t, err)
			}
			switch fault {
			case "payment-declined":
				require.Equal(t, "rejected", r.Status)
				require.Zero(t, r.Effects.Payments)
			case "ticket-failed", "refund-unknown":
				require.Equal(t, "refunded", r.Status)
				require.Equal(t, 1, r.Effects.Payments)
				require.Equal(t, 1, r.Effects.Refunds)
				require.Zero(t, r.Effects.Tickets)
			default:
				require.Equal(t, "ticketed", r.Status)
				require.Equal(t, 1, r.Effects.Payments)
				require.Equal(t, 1, r.Effects.Tickets)
			}
		})
	}
}
func TestConcurrentConfirmAndCancel(t *testing.T) {
	c := testConfig(t)
	r := selectOffer(t, c, begin(t, c))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cmd := command("confirm")
			cmd.Token = r.Confirmation.Token
			if i == 3 {
				cmd.Action = "cancel"
			}
			_, _ = Run(context.Background(), c, cmd)
		}(i)
	}
	wg.Wait()
	out, err := Run(context.Background(), c, command("status"))
	require.NoError(t, err)
	require.LessOrEqual(t, out.Effects.Payments, 1)
	require.LessOrEqual(t, out.Effects.Tickets, 1)
	if out.Status == "canceled" {
		require.Zero(t, out.Effects.Payments)
	} else {
		require.Contains(t, []string{"ticketed", "needs_attention"}, out.Status)
	}
}
func TestRequestValidationAndNoOffers(t *testing.T) {
	for _, bad := range []string{"missing", "past", "return", "child", "budget", "no-flights"} {
		t.Run(bad, func(t *testing.T) {
			c := testConfig(t)
			cmd := command("start")
			req := demoRequest()
			switch bad {
			case "missing":
				req.From = ""
			case "past":
				req.DepartDate = "2020-01-01"
			case "return":
				req.ReturnDate = "2030-01-01"
			case "child":
				req.Travelers[0].Kind = "child"
			case "budget":
				req.BudgetCents = 1
			case "no-flights":
				c.Scenario = "no-flights"
			}
			cmd.Input = &req
			r, err := Run(context.Background(), c, cmd)
			require.NoError(t, err)
			require.Contains(t, []string{"needs_input", "rejected"}, r.Status)
			require.Zero(t, r.Effects.Payments)
		})
	}
	c := testConfig(t)
	cmd := command("start")
	req := demoRequest()
	req.ReturnDate = "2030-01-15"
	req.Travelers = append(req.Travelers, Traveler{Ref: "demo-child-1", Kind: "child"})
	cmd.Input = &req
	r, err := Run(context.Background(), c, cmd)
	require.NoError(t, err)
	require.Len(t, r.Offers[0].Legs, 2)
}

func TestConfirmationRejectsReplacementInput(t *testing.T) {
	c := testConfig(t)
	r := selectOffer(t, c, begin(t, c))
	cmd := command("confirm")
	cmd.Token = r.Confirmation.Token
	replacement := demoRequest()
	replacement.DepartDate = "2030-01-11"
	cmd.Input = &replacement
	_, err := Run(context.Background(), c, cmd)
	require.Error(t, err, "do not ignore replacement data and charge an old itinerary")
	out, err := Run(context.Background(), c, command("status"))
	require.NoError(t, err)
	require.Zero(t, out.Effects.Payments)
}

func TestNewIssuanceRequiresValidHeldConditionsAfterPayment(t *testing.T) {
	for _, change := range []string{"expiry", "price", "currency", "rules", "sold-out"} {
		t.Run(change, func(t *testing.T) {
			c := testConfig(t)
			c.Scenario = "payment-unknown"
			r := confirm(t, c, selectOffer(t, c, begin(t, c)))
			require.Equal(t, "payment_pending", r.Status)
			require.Equal(t, 1, r.Effects.Payments)
			switch change {
			case "expiry":
				c.Now = func() time.Time { return time.Date(2029, 1, 2, 0, 0, 0, 0, time.UTC) }
			case "price":
				c.Market.PriceDelta = 100
			case "currency":
				c.Market.Currency = "USD"
			case "rules":
				c.Market.Rules = "changed"
			case "sold-out":
				c.Market.SoldOut = true
			}
			out, err := Run(context.Background(), c, command("status"))
			require.NoError(t, err)
			require.Equal(t, "refunded", out.Status)
			require.Equal(t, 1, out.Effects.Payments)
			require.Equal(t, 1, out.Effects.Refunds)
			require.Zero(t, out.Effects.Tickets)
		})
	}
}

func TestSameDayReturnChronology(t *testing.T) {
	c := testConfig(t)
	cmd := command("start")
	req := demoRequest()
	req.ReturnDate = req.DepartDate
	cmd.Input = &req
	r, err := Run(context.Background(), c, cmd)
	require.NoError(t, err)
	for _, q := range r.Offers {
		arrive, err := time.Parse(time.RFC3339, q.Legs[0].Arrival)
		require.NoError(t, err)
		depart, err := time.Parse(time.RFC3339, q.Legs[1].Departure)
		require.NoError(t, err)
		require.True(t, depart.After(arrive), "return cannot depart before outbound arrival")
	}
}
