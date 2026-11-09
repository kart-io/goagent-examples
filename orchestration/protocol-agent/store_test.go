package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kart-io/goagent/codec/json"
	agentErrors "github.com/kart-io/goagent/errors"
	mg "github.com/kart-io/goagent/management"
	"github.com/stretchr/testify/require"
)

func testDocument(t *testing.T, id, tenant string, revision int64, at time.Time) mg.Document {
	t.Helper()
	raw, err := json.Marshal(mg.Agent{Meta: mg.Meta{ID: id, TenantID: tenant, Revision: revision, CreatedAt: at}})
	require.NoError(t, err)
	return mg.Document{Kind: "agents", TenantID: tenant, ID: id, Revision: revision, CreatedAt: at, Data: raw}
}

func TestStoreRollbackCASAndIsolation(t *testing.T) {
	s := newDemoStore()
	ctx := context.Background()
	at := time.Now().UTC()
	d := testDocument(t, "one", "tenant-a", 1, at)
	failed := errors.New("事务中断")
	err := s.WithinTx(ctx, func(tx mg.Tx) error { require.NoError(t, tx.Insert(ctx, d)); return failed })
	require.ErrorIs(t, err, failed)
	_, err = s.Get(ctx, "agents", "tenant-a", "one")
	require.ErrorIs(t, err, agentErrors.ErrManagementNotFound)
	require.NoError(t, s.WithinTx(ctx, func(tx mg.Tx) error { return tx.Insert(ctx, d) }))
	d.Data[0] = '!'
	got, err := s.Get(ctx, "agents", "tenant-a", "one")
	require.NoError(t, err)
	require.True(t, json.Valid(got.Data))
	next := testDocument(t, "one", "tenant-a", 2, at)
	err = s.WithinTx(ctx, func(tx mg.Tx) error { return tx.CAS(ctx, next, 7) })
	require.ErrorIs(t, err, agentErrors.ErrManagementConflict)
	require.NoError(t, s.WithinTx(ctx, func(tx mg.Tx) error { return tx.CAS(ctx, next, 1) }))
	_, err = s.Get(ctx, "agents", "tenant-b", "one")
	require.ErrorIs(t, err, agentErrors.ErrManagementNotFound)
	got, err = s.Get(ctx, "agents", "tenant-a", "one")
	require.NoError(t, err)
	require.EqualValues(t, 2, got.Revision)
	got.Data[0] = '!'
	again, err := s.Get(ctx, "agents", "tenant-a", "one")
	require.NoError(t, err)
	require.True(t, json.Valid(again.Data))
}

func TestStorePaginationVisibilityAndCanceledCommit(t *testing.T) {
	s := newDemoStore()
	ctx := context.Background()
	at := time.Now().UTC()
	for _, id := range []string{"a", "b", "c"} {
		d := testDocument(t, id, "tenant", 1, at)
		require.NoError(t, s.WithinTx(ctx, func(tx mg.Tx) error { return tx.Insert(ctx, d) }))
	}
	q := mg.Selection{TenantID: "tenant", Visibility: mg.Visibility{All: true}, Query: mg.Query{Limit: 2}}
	page, err := s.List(ctx, "agents", q)
	require.NoError(t, err)
	require.Len(t, page, 2)
	require.Equal(t, "a", page[0].ID)
	q.Query.Cursor = at.Format(time.RFC3339Nano) + "|b"
	page, err = s.List(ctx, "agents", q)
	require.NoError(t, err)
	require.Len(t, page, 1)
	require.Equal(t, "c", page[0].ID)
	q.Query.Cursor = ""
	q.Visibility = mg.Visibility{ResourceIDs: []string{"b"}}
	page, err = s.List(ctx, "agents", q)
	require.NoError(t, err)
	require.Len(t, page, 1)
	require.Equal(t, "b", page[0].ID)
	canceled, cancel := context.WithCancel(ctx)
	err = s.WithinTx(canceled, func(tx mg.Tx) error {
		if err := tx.Delete(canceled, "agents", "tenant", "a", 1); err != nil {
			return err
		}
		cancel()
		return nil
	})
	require.ErrorIs(t, err, context.Canceled)
	_, err = s.Get(ctx, "agents", "tenant", "a")
	require.NoError(t, err)
}
