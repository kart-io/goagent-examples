package main

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReceiverAcknowledgesOnlyValidEvents(t *testing.T) {
	var output bytes.Buffer
	handler := newReceiver(&output)
	for _, tc := range []struct {
		method, body string
		status       int
	}{
		{"POST", `{"schema_version":1,"event_id":"evt-1","run_id":"run-1","event_type":"run.completed","data":{"result":"订单"}}`, 204},
		{"POST", `{"event_type":"run.completed"}`, 400},
		{"POST", `{`, 400},
		{"GET", ``, 405},
		{"POST", `{"schema_version":1,"event_id":"e","run_id":"r","event_type":"run.completed","data":"` + strings.Repeat("x", 1<<20) + `"}`, 413},
	} {
		req := httptest.NewRequest(tc.method, "/events", strings.NewReader(tc.body))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != tc.status {
			t.Fatalf("want %d, got %d: %s", tc.status, w.Code, w.Body.String())
		}
	}
	if strings.Count(output.String(), "evt-1") != 1 {
		t.Fatalf("only the valid event should be logged: %s", output.String())
	}
}
