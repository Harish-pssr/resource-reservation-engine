package httpapi

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strconv"
	"strings"
	"time"

	"resourcereservation/internal/checker"
)

// Read endpoints for the console (the web/ folder). They only read: nothing here changes a hold or a booking.

// maxItemsSnapshot caps one snapshot of an event's items, so a huge event cannot make a page heavy.
const maxItemsSnapshot = 5000

func (h *Handler) registerUI() {
	h.mux.HandleFunc("GET /events", h.listEvents)
	h.mux.HandleFunc("GET /events/{id}/items", h.eventItems)
	h.mux.HandleFunc("GET /events/{id}/bookings", h.eventBookings)
	h.mux.HandleFunc("GET /healthz", h.healthz)
	h.mux.Handle("GET /admin/check", h.tokenGuard(http.HandlerFunc(h.runCheck)))
}

// tokenGuard needs the admin token but, unlike the fault controls, not FAULTS_ENABLED: the checker only reads.
func (h *Handler) tokenGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case h.d.AdminToken == "":
			fail(w, http.StatusForbidden, "no admin token is configured (start the server with ADMIN_TOKEN)")
		case subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")), []byte(h.d.AdminToken)) != 1:
			fail(w, http.StatusUnauthorized, "missing or invalid admin token")
		default:
			next.ServeHTTP(w, r)
		}
	})
}

func (h *Handler) eventID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || h.d.Pool == nil {
		fail(w, http.StatusBadRequest, "bad event id")
		return 0, false
	}
	return id, true
}

// listEvents returns the most recent events with how many items are free, held and confirmed right now.
// "Held" excludes expired holds: they count as free (D3).
func (h *Handler) listEvents(w http.ResponseWriter, r *http.Request) {
	if h.d.Pool == nil {
		fail(w, http.StatusNotFound, "no database")
		return
	}
	rows, err := h.d.Pool.Query(r.Context(), `
		SELECT e.id, e.name, e.capacity, e.created_at,
		       count(*) FILTER (WHERE i.state = 'confirmed'),
		       count(*) FILTER (WHERE i.state = 'held' AND i.hold_expires_at >= clock_timestamp())
		FROM events e JOIN items i ON i.event_id = e.id
		GROUP BY e.id ORDER BY e.id DESC LIMIT 30`)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id int64
		var name string
		var capacity, confirmed, held int
		var created time.Time
		if err := rows.Scan(&id, &name, &capacity, &created, &confirmed, &held); err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		out = append(out, map[string]any{"id": id, "name": name, "capacity": capacity, "created_at": created,
			"confirmed": confirmed, "held": held, "free": capacity - confirmed - held})
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out})
}

// eventItems returns one snapshot of an event's items for the seat map. The state is the effective one:
// an expired hold is reported as free, exactly as the strategies treat it. expires_in_ms is computed on the
// DATABASE clock, so a browser with a wrong clock still shows a correct countdown.
func (h *Handler) eventItems(w http.ResponseWriter, r *http.Request) {
	id, ok := h.eventID(w, r)
	if !ok {
		return
	}
	var name string
	var capacity int
	if err := h.d.Pool.QueryRow(r.Context(), `SELECT name, capacity FROM events WHERE id = $1`, id).Scan(&name, &capacity); err != nil {
		fail(w, http.StatusNotFound, "no such event")
		return
	}
	rows, err := h.d.Pool.Query(r.Context(), `
		SELECT id, seq,
		       CASE WHEN state = 'held' AND hold_expires_at < clock_timestamp() THEN 'free' ELSE state END,
		       CASE WHEN state = 'held' AND hold_expires_at >= clock_timestamp() THEN holder END,
		       hold_version,
		       CASE WHEN state = 'held' AND hold_expires_at >= clock_timestamp()
		            THEN (extract(epoch FROM hold_expires_at - clock_timestamp()) * 1000)::bigint END
		FROM items WHERE event_id = $1 ORDER BY seq LIMIT $2`, id, maxItemsSnapshot)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var itemID, version int64
		var seq int
		var state string
		var holder *string
		var expires *int64
		if err := rows.Scan(&itemID, &seq, &state, &holder, &version, &expires); err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		it := map[string]any{"id": itemID, "seq": seq, "state": state, "version": version}
		if holder != nil {
			it["holder"] = *holder
		}
		if expires != nil {
			it["expires_in_ms"] = *expires
		}
		items = append(items, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"event": map[string]any{"id": id, "name": name, "capacity": capacity},
		"items": items, "truncated": capacity > len(items)})
}

// eventBookings returns the most recent confirmed bookings of an event.
func (h *Handler) eventBookings(w http.ResponseWriter, r *http.Request) {
	id, ok := h.eventID(w, r)
	if !ok {
		return
	}
	limit := 50
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 && n <= 200 {
		limit = n
	}
	rows, err := h.d.Pool.Query(r.Context(), `
		SELECT b.id, i.seq, b.buyer_id, b.hold_version, b.created_at, COALESCE(i.hold_request_id, '')
		FROM bookings b JOIN items i ON i.id = b.item_id
		WHERE b.event_id = $1 AND b.status = 'confirmed' ORDER BY b.id DESC LIMIT $2`, id, limit)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var bid, version int64
		var seq int
		var buyer, request string
		var at time.Time
		if err := rows.Scan(&bid, &seq, &buyer, &version, &at, &request); err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		out = append(out, map[string]any{"id": bid, "seat": seq, "buyer": buyer, "hold_version": version, "at": at, "request": request})
	}
	writeJSON(w, http.StatusOK, map[string]any{"bookings": out})
}

// healthz reports whether the stores answer, and how the server was configured, so the console can show it.
func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	status := func(err error) string {
		if err != nil {
			return "down: " + err.Error()
		}
		return "ok"
	}
	body := map[string]any{"faults_enabled": h.d.Faults.Enabled(), "waiting_room": h.d.Room != nil, "config": h.d.Info,
		"admin_token_configured": h.d.AdminToken != ""}
	healthy := true
	if h.d.Pool != nil {
		err := h.d.Pool.Ping(ctx)
		body["postgres"] = status(err)
		healthy = healthy && err == nil
	}
	if h.d.RedisPing != nil {
		err := h.d.RedisPing(ctx)
		body["redis"] = status(err)
		// Redis is a hint, not the system of record: the service is degraded, not down, without it.
		body["redis_is_optional"] = true
	}
	code := http.StatusOK
	if !healthy {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, body)
}

// runCheck runs the independent checker (the same code the benchmarks use) against the live database.
func (h *Handler) runCheck(w http.ResponseWriter, r *http.Request) {
	if h.d.Pool == nil {
		fail(w, http.StatusNotFound, "no database")
		return
	}
	rep, err := checker.Check(r.Context(), h.d.Pool)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": rep.OK(), "report": rep, "checked_at": time.Now().UTC()})
}
