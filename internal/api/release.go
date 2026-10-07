package api

import (
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
)

func (a *App) listReleaseDrafts(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.Releases.Drafts())
}

func (a *App) getReleaseDraft(w http.ResponseWriter, r *http.Request) {
	draft, ok := a.Releases.Draft(chi.URLParam(r, "draftID"))
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("release draft %s not found", chi.URLParam(r, "draftID")))
		return
	}
	writeJSON(w, http.StatusOK, draft)
}

func (a *App) createReleaseDraft(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Targets map[string]float64 `json:"targets"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	draft, err := a.Releases.GenerateDraft(input.Targets)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, draft)
}

func (a *App) approveReleaseDraft(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ZoneID string `json:"zone_id"`
		Actor  string `json:"actor"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if input.ZoneID == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("zone_id is required"))
		return
	}
	draft, err := a.Releases.ApproveZone(chi.URLParam(r, "draftID"), input.ZoneID, input.Actor)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, draft)
}

func (a *App) publishReleaseDraft(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Actor string `json:"actor"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	release, err := a.Releases.Publish(chi.URLParam(r, "draftID"), input.Actor)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusAccepted, release)
}

func (a *App) listReleases(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.Releases.Releases())
}

func (a *App) getRelease(w http.ResponseWriter, r *http.Request) {
	release, ok := a.Releases.Release(chi.URLParam(r, "releaseID"))
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("release %s not found", chi.URLParam(r, "releaseID")))
		return
	}
	writeJSON(w, http.StatusOK, release)
}

func (a *App) recordReleaseReceipt(w http.ResponseWriter, r *http.Request) {
	var input struct {
		StationID    string `json:"station_id"`
		AppliedEpoch uint64 `json:"applied_epoch"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if input.StationID == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("station_id is required"))
		return
	}
	release, err := a.Releases.Acknowledge(chi.URLParam(r, "releaseID"), input.StationID, input.AppliedEpoch)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, release)
}

func (a *App) reconcileRelease(w http.ResponseWriter, r *http.Request) {
	release, err := a.Releases.Reconcile(chi.URLParam(r, "releaseID"))
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, release)
}
