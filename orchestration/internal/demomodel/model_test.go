package demomodel

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCallWithKeepsArguments(t *testing.T) {
	c := CallWith("c1", "get_order", `{"order_id":"A1001"}`)
	require.Equal(t, "c1", c.ID)
	require.Equal(t, "function", c.Type)
	require.Equal(t, "get_order", c.Function.Name)
	require.JSONEq(t, `{"order_id":"A1001"}`, c.Function.Arguments)
}

func TestCallUsesEmptyArguments(t *testing.T) {
	c := Call("c2", "list_flights")
	require.Equal(t, "list_flights", c.Function.Name)
	require.Equal(t, "{}", c.Function.Arguments)
}
