package handler

import (
	"net/http"
	"time"

	"github.com/Paca-AI/api/internal/platform/delivery"
)

// enterJSONDelivery keeps current authorization and the actual JSON flush in
// one short lease. Call only around reads, never around authority mutations.
func enterJSONDelivery(gate *delivery.Gate, w http.ResponseWriter, r *http.Request) (*http.Request, func()) {
	if gate == nil {
		http.Error(w, "JSON delivery unavailable", 503)
		return nil, func() {}
	}
	ctx, _, leave, err := gate.Enter(r.Context())
	if err != nil {
		// The coordinator publishes open before returning, and local subscription
		// delivery is asynchronous. Bound the wait, then recheck under the lease.
		timer := time.NewTimer(fileWriteTimeout)
		defer timer.Stop()
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
	retry:
		for err != nil {
			select {
			case <-r.Context().Done():
				break retry
			case <-timer.C:
				break retry
			case <-tick.C:
				ctx, _, leave, err = gate.Enter(r.Context())
			}
		}
	}
	if err != nil {
		http.Error(w, "JSON delivery unavailable", 503)
		return nil, func() {}
	}
	controller := http.NewResponseController(w)
	if err = controller.SetWriteDeadline(time.Now().Add(fileWriteTimeout)); err != nil {
		leave()
		http.Error(w, "JSON delivery unavailable", 503)
		return nil, func() {}
	}
	w.Header().Set("Cache-Control", "private, no-store")
	return r.WithContext(ctx), func() {
		defer leave()
		defer controller.SetWriteDeadline(time.Time{})
		// Small JSON bodies remain in net/http's buffer until Flush. Release only
		// after actual delivery, so a revocation cannot commit before that flush.
		if err := controller.Flush(); err != nil {
			panic(http.ErrAbortHandler)
		}
	}
}
