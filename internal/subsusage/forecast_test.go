package subsusage

import (
	"testing"
	"time"
)

func TestRecordForecastFirstPerAccountWins(t *testing.T) {
	h := &History{data: newHistoryData()}
	h.data.Providers["a"], h.data.Providers["b"], h.data.Providers["x"] = "claude", "claude", "codex"
	v := func(f float64) *float64 { return &f }
	now := time.Date(2026, 11, 12, 10, 20, 0, 0, time.UTC)
	if !h.RecordForecast(ForecastPost{Accounts: map[string]forecastSnap{"a": {W: [4]*float64{v(50), nil, nil, nil}}, "x": {W: [4]*float64{v(1), nil, nil, nil}}}}, now) {
		t.Fatal("first post not stored")
	}
	// Same hour: "a" keeps its first value, "b" is added, out-of-range values are clamped.
	h.RecordForecast(ForecastPost{Accounts: map[string]forecastSnap{"a": {W: [4]*float64{v(10), nil, nil, nil}}, "b": {W: [4]*float64{v(130), nil, nil, nil}}}}, now.Add(20*time.Minute))
	got := h.data.Forecasts[now.Truncate(time.Hour).Unix()]
	if *got["a"].W[0] != 50 || *got["b"].W[0] != 100 || got["x"] != nil {
		t.Fatalf("got a=%v b=%v x=%v", *got["a"].W[0], *got["b"].W[0], got["x"])
	}
	// Snapshots older than forecastKeep are dropped on the next store.
	h.RecordForecast(ForecastPost{Accounts: map[string]forecastSnap{"a": {W: [4]*float64{v(1), nil, nil, nil}}}}, now.Add(forecastKeep+2*time.Hour))
	if _, ok := h.data.Forecasts[now.Truncate(time.Hour).Unix()]; ok {
		t.Fatal("old snapshot kept")
	}
}

func TestIsModelRequest(t *testing.T) {
	for path, want := range map[string]bool{"/v1/messages": true, "/v1/messages/count_tokens": false, "/v1/chat/completions": false, "/backend-api/codex/responses": false} {
		if got := isModelRequest("POST", path); got != want {
			t.Errorf("%s: %v", path, got)
		}
	}
	if isModelRequest("GET", "/v1/messages") {
		t.Error("GET counted")
	}
}
