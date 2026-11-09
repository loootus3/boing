package subsusage

import "testing"

func TestAddFableShare(t *testing.T) {
	body := []byte(`{"seven_day":{"utilization":60,"resets_at":null},
	 "limits":[{"kind":"session","percent":3},{"kind":"weekly_all","percent":60},
	  {"kind":"weekly_scoped","percent":40,"scope":{"model":{"display_name":"Fable"}}}],
	 "weekly_scoped_shares":[{"limit_index":2,"used_percent_of_weekly":20,"allowance_percent_of_weekly":50}]}`)
	var e Entry
	if err := parseClaudeUsage(body, &e); err != nil {
		t.Fatal(err)
	}
	w := e.Windows[0]
	if w.Kind != "weekly" || w.FablePercent == nil || *w.FablePercent != 20 || *w.FableAllowance != 50 || *w.FableLimitPercent != 40 {
		t.Fatalf("weekly = %+v", w)
	}
	var none Entry
	if err := parseClaudeUsage([]byte(`{"seven_day":{"utilization":5}}`), &none); err != nil || none.Windows[0].FablePercent != nil {
		t.Fatalf("no Fable limit: %+v %v", none.Windows, err)
	}
}
