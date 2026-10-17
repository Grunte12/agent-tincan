package relay

import (
	"errors"
	"net/http"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

var errAggregateNotificationsScoped = errors.New("session-scoped callers cannot use aggregate notifications")

type notificationsResponse struct {
	Notifications []store.Notification `json:"notifications"`
	Waiting       int                  `json:"waiting"`
	Queued        int                  `json:"queued"`
}

func (s *Server) handleNotifications(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(client.SessionHeader) != "" {
		writeErr(w, http.StatusForbidden, errAggregateNotificationsScoped)
		return
	}
	name := s.agent(w, r)
	if name == "" {
		return
	}
	hold := durationParam(r, "hold", s.cfg.PollHold, s.cfg.PollHold)
	deadline := time.NewTimer(hold)
	defer deadline.Stop()
	for {
		wake := s.hub.wait(inboxKey(name))
		items, err := s.store.Notifications(r.Context(), name)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if len(items) > 0 {
			out := notificationsResponse{Notifications: items, Waiting: len(items)}
			for _, item := range items {
				if item.Kind == "request" {
					out.Queued++
				}
			}
			writeJSON(w, http.StatusOK, out)
			return
		}
		select {
		case <-wake:
		case <-deadline.C:
			writeJSON(w, http.StatusOK, notificationsResponse{Notifications: []store.Notification{}})
			return
		case <-s.stopping:
			w.WriteHeader(http.StatusNoContent)
			return
		case <-r.Context().Done():
			return
		}
	}
}
