package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/kart-io/goagent-examples/internal/livemodel"
)

func execute(ctx context.Context, args []string, w io.Writer) error {
	flags := flag.NewFlagSet("flight-booking", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var c Config
	var cmd Command
	var input, at, crash string
	var live bool
	flags.StringVar(&c.Dir, "dir", "", "required trusted local demo state directory")
	flags.StringVar(&cmd.ID, "request", "booking-1", "business request ID")
	flags.StringVar(&cmd.Actor, "actor", "demo-traveler", "synthetic actor; not authentication")
	flags.StringVar(&cmd.Action, "action", "status", "start/message/compare/select/confirm/status/revise/cancel")
	flags.IntVar(&cmd.Revision, "revision", 1, "revision required for selection, confirmation, revision and cancel")
	flags.StringVar(&cmd.Token, "token", "", "explicit confirmation token from previous result")
	flags.StringVar(&cmd.QuoteID, "quote", "", "quote ID for first confirmation")
	flags.StringVar(&cmd.Message, "message", "", "user message; offline mode accepts documented fixed transcripts")
	flags.StringVar(&input, "input", "", "complete synthetic request JSON file")
	flags.StringVar(&c.Scenario, "scenario", "normal", "new request scenario: normal/no-flights/payment-declined/payment-unknown/ticket-unknown/ticket-failed/refund-unknown")
	flags.Int64Var(&c.Market.PriceDelta, "price-delta", 0, "synthetic market price change in cents")
	flags.StringVar(&c.Market.Currency, "currency", "", "synthetic replacement currency")
	flags.StringVar(&c.Market.Rules, "rules", "", "synthetic replacement rule version")
	flags.BoolVar(&c.Market.SoldOut, "sold-out", false, "simulate inventory exhaustion")
	flags.BoolVar(&live, "live", false, "explicitly enable paid real model; business remains simulated")
	flags.StringVar(&at, "at", "", "inject synthetic clock RFC3339; default actual UTC time")
	flags.StringVar(&crash, "crash-after", "", "exit 86 after supplier commits payment/ticket/refund before workflow checkpoint")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	if input != "" {
		b, err := os.ReadFile(input)
		if err != nil {
			return err
		}
		if len(b) > 16000 {
			return fmt.Errorf("input too large")
		}
		var r Request
		if err = strictJSON(string(b), &r); err != nil {
			return err
		}
		cmd.Input = &r
	}
	if at != "" {
		now, err := time.Parse(time.RFC3339, at)
		if err != nil {
			return fmt.Errorf("invalid synthetic clock")
		}
		c.Now = func() time.Time { return now }
	}
	if crash != "" {
		if crash != "payment" && crash != "ticket" && crash != "refund" {
			return fmt.Errorf("invalid crash stage")
		}
		c.AfterCommit = func(stage string) {
			if stage == crash {
				os.Exit(86)
			}
		}
	}
	if live {
		var err error
		c.Client, err = livemodel.FromEnv()
		if err != nil {
			return err
		}
	}
	out, err := Run(ctx, c, cmd)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := execute(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, livemodel.Redact(err.Error()))
		os.Exit(1)
	}
}
