package main

import (
	"context"
	"fmt"
	"github.com/kart-io/goagent/core/durable"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCrashChild(t *testing.T) {
	if os.Getenv("FLIGHT_CRASH_CHILD") != "1" {
		return
	}
	stage := os.Getenv("FLIGHT_CRASH_STAGE")
	c := Config{Dir: os.Getenv("FLIGHT_CRASH_DIR"), Now: func() time.Time { return time.Date(2029, 1, 1, 0, 0, 0, 0, time.UTC) }, AfterCommit: func(actual string) {
		if actual == stage {
			os.Exit(86)
		}
	}}
	cmd := command("confirm")
	cmd.Token = os.Getenv("FLIGHT_CRASH_TOKEN")
	_, err := Run(context.Background(), c, cmd)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(3)
}
func TestFreshProcessRecoversSupplierCommit(t *testing.T) {
	for _, stage := range []string{"payment", "ticket", "refund"} {
		t.Run(stage, func(t *testing.T) {
			c := testConfig(t)
			if stage == "refund" {
				c.Scenario = "ticket-failed"
			}
			r := selectOffer(t, c, begin(t, c))
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCrashChild$")
			child.Env = append(os.Environ(), "FLIGHT_CRASH_CHILD=1", "FLIGHT_CRASH_DIR="+c.Dir, "FLIGHT_CRASH_STAGE="+stage, "FLIGHT_CRASH_TOKEN="+r.Confirmation.Token)
			b, err := child.CombinedOutput()
			var exit *exec.ExitError
			require.ErrorAs(t, err, &exit, string(b))
			require.Equal(t, 86, exit.ExitCode(), string(b))
			// Approval is now expired; the committed payment must still be found.
			c.Now = func() time.Time { return time.Date(2029, 1, 2, 0, 0, 0, 0, time.UTC) }
			out, err := Run(context.Background(), c, command("status"))
			require.NoError(t, err)
			require.Equal(t, 1, out.Effects.Payments)
			if stage == "refund" || stage == "payment" {
				require.Equal(t, "refunded", out.Status)
				require.Equal(t, 1, out.Effects.Refunds)
			} else {
				require.Equal(t, "ticketed", out.Status)
				require.Equal(t, 1, out.Effects.Tickets)
			}
			again, err := Run(context.Background(), c, command("status"))
			require.NoError(t, err)
			require.Equal(t, out.Effects, again.Effects)
		})
	}
}
func TestReplayedSelectionCannotEscapeFinalWait(t *testing.T) {
	c := testConfig(t)
	first := begin(t, c)
	final := selectOffer(t, c, first)
	again := selectOffer(t, c, first)
	require.Equal(t, final.Confirmation.Token, again.Confirmation.Token)
	require.Zero(t, again.Effects.Payments)
}
func TestExpiredSelectionDoesNotKeepHold(t *testing.T) {
	c := testConfig(t)
	r := begin(t, c)
	c.Now = func() time.Time { return time.Date(2029, 1, 2, 0, 0, 0, 0, time.UTC) }
	cmd := command("select")
	cmd.Token = r.Confirmation.Token
	cmd.QuoteID = r.Offers[0].ID
	out, err := Run(context.Background(), c, cmd)
	require.NoError(t, err)
	require.Equal(t, "requote_required", out.Status)
	require.Zero(t, out.Effects.Holds)
}

// Reproduce a process loss after the host saved final authorization but before
// it supplied the internal workflow approval key. No supplier payment exists.
func TestAcceptedAuthorizationExpiresBeforeGraphAdvances(t *testing.T) {
	c := testConfig(t)
	_ = selectOffer(t, c, begin(t, c))
	store, err := durable.NewFileStore(filepath.Join(c.Dir, "business"))
	require.NoError(t, err)
	o, found, err := loadRecord[booking](context.Background(), store, "booking-1", "flight-booking")
	require.NoError(t, err)
	require.True(t, found)
	o.FinalAccepted = true
	approved := c.now()
	o.Final.ApprovedAt = &approved
	o.Final.ApprovedBy = o.Owner
	require.NoError(t, saveRecord(context.Background(), store, o.ID, "flight-booking", o))
	c.Now = func() time.Time { return time.Date(2029, 1, 2, 0, 0, 0, 0, time.UTC) }
	out, err := Run(context.Background(), c, command("status"))
	require.NoError(t, err)
	require.Equal(t, "requote_required", out.Status)
	require.Zero(t, out.Effects.Payments)
	out, err = Run(context.Background(), c, command("status"))
	require.NoError(t, err)
	require.Equal(t, "requote_required", out.Status)
}
