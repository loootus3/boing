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

func TestRecordCurveKeepsFirstAndThins(t *testing.T) {
	h := &History{data: newHistoryData()}
	h.data.Providers["a"], h.data.Providers["x"] = "claude", "codex"
	now := time.Date(2026, 11, 12, 10, 20, 0, 0, time.UTC)
	post := func(at time.Time, v float64) bool {
		return h.RecordForecast(ForecastPost{Curve: &CurvePost{Step: 7200, Pace: 2.5, Accs: map[string][]float64{"a": {v, v - 1, 130}, "x": {1, 2}}}}, at)
	}
	if !post(now, 50) {
		t.Fatal("curve not stored")
	}
	if post(now.Add(10*time.Minute), 10) {
		t.Fatal("second curve of the hour stored")
	}
	c := h.data.Curves[now.Truncate(time.Hour).Unix()]
	if got := c.Accs["a"]; len(got) != 3 || got[0] != 50 || got[2] != 100 || c.Accs["x"] != nil {
		t.Fatalf("got %+v", c.Accs)
	}
	if h.RecordForecast(ForecastPost{Curve: &CurvePost{Step: 60, Accs: map[string][]float64{"a": {1, 2}}}}, now.Add(time.Hour)) {
		t.Fatal("bad step accepted")
	}
	// After curveDenseKeep, only hours divisible by 6 stay (10:00 UTC is not one).
	post(now.Add(curveDenseKeep+2*time.Hour), 40)
	if _, ok := h.data.Curves[now.Truncate(time.Hour).Unix()]; ok {
		t.Fatal("old off-grid curve kept")
	}
}
