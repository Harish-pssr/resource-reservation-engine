package httpapi_test

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func (f *faultFixture) get(path, token string) (int, map[string]any) {
	f.t.Helper()
	return f.call("GET", path, nil, token)
}

func TestItemsSnapshotReportsEffectiveStateAndTheDatabaseClockCountdown(t *testing.T) {
	f := newFaultFixture(t, false, adminToken, time.Millisecond)
	f.holdItem("alice", "r1", f.items[0])
	_, _, refs := f.holdItem("bob", "r2", f.items[1])
	f.confirm("bob", refs)

	code, out := f.get("/events/"+itoa(1)+"/items", "")
	if code != http.StatusOK {
		t.Fatalf("%d %v", code, out)
	}
	items := out["items"].([]any)
	if len(items) != 6 || out["truncated"] != false {
		t.Fatalf("%d items, truncated=%v", len(items), out["truncated"])
	}
	first, second, third := items[0].(map[string]any), items[1].(map[string]any), items[2].(map[string]any)
	if first["state"] != "held" || first["holder"] != "alice" {
		t.Fatalf("alice's hold: %v", first)
	}
	if ms, _ := first["expires_in_ms"].(float64); ms < 50000 || ms > 60000 { // HoldTTL is one minute
		t.Fatalf("countdown %v ms should be just under a minute, on the database clock", first["expires_in_ms"])
	}
	if second["state"] != "confirmed" || second["holder"] != nil {
		t.Fatalf("a confirmed seat shows no holder and no countdown: %v", second)
	}
	if third["state"] != "free" {
		t.Fatalf("%v", third)
	}
}

// An expired hold is free (D3): the snapshot must say so, exactly as the strategies treat it.
func TestSnapshotShowsAnExpiredHoldAsFree(t *testing.T) {
	f := newFaultFixture(t, false, adminToken, time.Millisecond)
	f.holdItem("alice", "r1", f.items[0])
	if _, err := f.pool.Exec(context.Background(), `UPDATE items SET hold_expires_at = clock_timestamp() - interval '1 second' WHERE id = $1`, f.items[0]); err != nil {
		t.Fatal(err)
	}
	_, out := f.get("/events/1/items", "")
	it := out["items"].([]any)[0].(map[string]any)
	if it["state"] != "free" || it["holder"] != nil || it["expires_in_ms"] != nil {
		t.Fatalf("an expired hold must read as free with no holder: %v", it)
	}
	_, ev := f.get("/events", "")
	e := ev["events"].([]any)[0].(map[string]any)
	if e["held"] != float64(0) || e["free"] != float64(6) {
		t.Fatalf("the event list must agree: %v", e)
	}
}

func TestEventListCountsFreeHeldAndConfirmed(t *testing.T) {
	f := newFaultFixture(t, false, adminToken, time.Millisecond)
	f.holdItem("alice", "r1", f.items[0])
	_, _, refs := f.holdItem("bob", "r2", f.items[1])
	f.confirm("bob", refs)
	_, out := f.get("/events", "")
	e := out["events"].([]any)[0].(map[string]any)
	if e["capacity"] != float64(6) || e["held"] != float64(1) || e["confirmed"] != float64(1) || e["free"] != float64(4) {
		t.Fatalf("%v", e)
	}
}

func TestBookingsListIsNewestFirst(t *testing.T) {
	f := newFaultFixture(t, false, adminToken, time.Millisecond)
	for i, buyer := range []string{"a", "b", "c"} {
		_, _, refs := f.holdItem(buyer, buyer+"r", f.items[i])
		f.confirm(buyer, refs)
	}
	_, out := f.get("/events/1/bookings?limit=2", "")
	b := out["bookings"].([]any)
	if len(b) != 2 || b[0].(map[string]any)["buyer"] != "c" || b[1].(map[string]any)["buyer"] != "b" {
		t.Fatalf("%v", b)
	}
}

func TestUnknownEventIs404AndBadIDIs400(t *testing.T) {
	f := newFaultFixture(t, false, adminToken, time.Millisecond)
	if code, _ := f.get("/events/99999/items", ""); code != http.StatusNotFound {
		t.Fatalf("%d", code)
	}
	if code, _ := f.get("/events/abc/items", ""); code != http.StatusBadRequest {
		t.Fatalf("%d", code)
	}
}

func TestHealthzReportsStoresAndConfiguration(t *testing.T) {
	f := newFaultFixture(t, false, adminToken, time.Millisecond)
	code, out := f.get("/healthz", "")
	if code != http.StatusOK || out["postgres"] != "ok" {
		t.Fatalf("%d %v", code, out)
	}
	if out["faults_enabled"] != false || out["admin_token_configured"] != true {
		t.Fatalf("%v", out)
	}
}

func TestCheckEndpointNeedsTheTokenButNotFaultsEnabled(t *testing.T) {
	f := newFaultFixture(t, false, adminToken, time.Millisecond) // faults disabled
	if code, _ := f.get("/admin/check", ""); code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", code)
	}
	if code, _ := f.get("/admin/check", "wrong"); code != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", code)
	}
	code, out := f.get("/admin/check", adminToken)
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("%d %v", code, out)
	}
	if rep := out["report"].(map[string]any); rep["double_sold_items"] != float64(0) {
		t.Fatalf("%v", rep)
	}
	none := newFaultFixture(t, false, "", time.Millisecond)
	if code, _ := none.get("/admin/check", adminToken); code != http.StatusForbidden {
		t.Fatalf("with no token configured the endpoint must refuse everyone: %d", code)
	}
}
