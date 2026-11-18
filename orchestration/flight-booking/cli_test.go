package main

import (
	"bytes"
	"context"
	"testing"

	"github.com/kart-io/goagent/codec/json"

	"github.com/stretchr/testify/require"
)

func TestCLIStartAndExplicitConfirmations(t *testing.T) {
	var output bytes.Buffer
	require.NoError(t, execute(context.Background(), []string{"-dir", t.TempDir(), "-action", "message", "-message", "我要订机票"}, &output))
	var r Result
	require.NoError(t, json.Unmarshal(output.Bytes(), &r))
	require.Equal(t, "needs_input", r.Status)
	require.Equal(t, 1, r.ModelCalls)
	output.Reset()
	require.Error(t, execute(context.Background(), []string{"-dir", t.TempDir(), "-action", "confirm"}, &output))
}
