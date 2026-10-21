package api

import (
	"errors"
	"net/http"

	"github.com/knotra-dev/engine/internal/store/db"
)

var errRestartPrepared = errors.New("managed engine restart prepared")

// HoldManagedAdmission keeps a replacement process idle until its applied
// source snapshot is durably committed by the desktop owner.
func (s *Server) HoldManagedAdmission() {
	s.admissionMu.Lock()
	s.restartPrepared = true
	s.admissionMu.Unlock()
}

func (s *Server) prepareRestart(w http.ResponseWriter, r *http.Request) {
	var request struct{}
	if _, err := readBody(w, r, &request); err != nil {
		s.fail(w, 400, "INPUT_INVALID", err.Error(), nil)
		return
	}
	// Run admission holds the read lock through its transaction commit, so no
	// admitted run can appear between this idle check and the managed restart.
	s.admissionMu.Lock()
	defer s.admissionMu.Unlock()
	if r.Context().Err() != nil {
		s.fail(w, 503, "UNAVAILABLE", "engine control request was cancelled", nil)
		return
	}
	active, err := db.New(s.Store.Pool).CountUnfinishedRuns(r.Context())
	if err != nil {
		s.err(w, err)
		return
	}
	if r.Context().Err() != nil {
		s.fail(w, 503, "UNAVAILABLE", "engine control request was cancelled", nil)
		return
	}
	if active == 0 {
		s.restartPrepared = true
	}
	s.write(w, 200, map[string]any{"ready": active == 0, "activeRuns": active})
}

func (s *Server) cancelRestart(w http.ResponseWriter, r *http.Request) {
	var request struct{}
	if _, err := readBody(w, r, &request); err != nil {
		s.fail(w, 400, "INPUT_INVALID", err.Error(), nil)
		return
	}
	s.admissionMu.Lock()
	defer s.admissionMu.Unlock()
	if r.Context().Err() != nil {
		s.fail(w, 503, "UNAVAILABLE", "engine control request was cancelled", nil)
		return
	}
	s.restartPrepared = false
	s.write(w, 200, map[string]any{"accepted": true})
}
