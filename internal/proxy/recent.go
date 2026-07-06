package proxy

import (
	"net/http"
	"sync"
	"time"

	"github.com/Lore-Hex/BurstyRouter/internal/policy"
)

const recentDecisionCap = 100

type recentDecision struct {
	At     string `json:"at"`
	Path   string `json:"path"`
	Route  string `json:"route"`
	Reason string `json:"reason"`
	Status int    `json:"status"`
}

type recentDecisions struct {
	mu    sync.Mutex
	items [recentDecisionCap]recentDecision
	next  int
	count int
}

func (r *recentDecisions) add(decision recentDecision) {
	if decision.Route == "" || decision.Reason == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items[r.next] = decision
	r.next = (r.next + 1) % recentDecisionCap
	if r.count < recentDecisionCap {
		r.count++
	}
}

func (r *recentDecisions) snapshot(limit int) []recentDecision {
	r.mu.Lock()
	defer r.mu.Unlock()
	if limit <= 0 || limit > r.count {
		limit = r.count
	}
	out := make([]recentDecision, 0, limit)
	for i := 0; i < limit; i++ {
		idx := (r.next - 1 - i + recentDecisionCap) % recentDecisionCap
		out = append(out, r.items[idx])
	}
	return out
}

func (s *stats) recordRecentDecision(path string, status int, route policy.Route, reason policy.Reason) {
	if s == nil {
		return
	}
	s.recent.add(recentDecision{
		At:     time.Now().UTC().Format(time.RFC3339),
		Path:   path,
		Route:  string(route),
		Reason: string(reason),
		Status: status,
	})
}

type recentDecisionWriter struct {
	http.ResponseWriter
	stats *stats
	path  string
	wrote bool
}

func (w *recentDecisionWriter) WriteHeader(status int) {
	if !w.wrote {
		w.wrote = true
		route := policy.Route(w.Header().Get("X-Bursty-Route"))
		reason := policy.Reason(w.Header().Get("X-Bursty-Reason"))
		w.stats.recordRecentDecision(w.path, status, route, reason)
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *recentDecisionWriter) Write(p []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

func (w *recentDecisionWriter) Flush() {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}
