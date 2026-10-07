package api

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"heatpulse/internal/release"
)

func (a *App) listTargetDrafts(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.Releases.Drafts())
}

func (a *App) createTargetDraft(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Targets map[string]float64 `json:"targets"`
		Actor   string             `json:"actor"`
		Note    string             `json:"note"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	draft, err := a.Releases.CreateDraft(input.Targets, input.Actor, input.Note)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, draft)
}

func (a *App) getTargetDraft(w http.ResponseWriter, r *http.Request) {
	draft, ok := a.Releases.Draft(chi.URLParam(r, "draftID"))
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("target draft not found"))
		return
	}
	writeJSON(w, http.StatusOK, draft)
}

func (a *App) refreshTargetDraft(w http.ResponseWriter, r *http.Request) {
	draft, err := a.Releases.RefreshDraft(chi.URLParam(r, "draftID"))
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, draft)
}

func (a *App) listReleases(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.Releases.Releases())
}

func (a *App) createRelease(w http.ResponseWriter, r *http.Request) {
	var input struct {
		DraftID string `json:"draft_id"`
		Actor   string `json:"actor"`
		Note    string `json:"note"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	created, err := a.Releases.CreateRelease(input.DraftID, input.Actor, input.Note)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (a *App) getRelease(w http.ResponseWriter, r *http.Request) {
	created, ok := a.Releases.Release(chi.URLParam(r, "releaseID"))
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("release not found"))
		return
	}
	writeJSON(w, http.StatusOK, created)
}

func (a *App) approveReleaseZone(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Actor string `json:"actor"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	created, err := a.Releases.ApproveZone(chi.URLParam(r, "releaseID"), chi.URLParam(r, "zoneID"), input.Actor)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, created)
}

func (a *App) rejectReleaseZone(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Actor  string `json:"actor"`
		Reason string `json:"reason"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	created, err := a.Releases.RejectZone(chi.URLParam(r, "releaseID"), chi.URLParam(r, "zoneID"), input.Actor, input.Reason)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, created)
}

// publishRelease maps the aggregate status to a status code that can never
// be mistaken for success: 200 only when every controller acknowledged,
// 207 when the batch ran but one or more controllers failed, 409 when the
// batch could not run (missing approval, stale revision, terminal state).
func (a *App) publishRelease(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Actor string `json:"actor"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	created, err := a.Releases.Publish(chi.URLParam(r, "releaseID"), input.Actor)
	if err != nil {
		// Results (for example an expired revision) are persisted and can
		// be fetched from GET /releases/{id}; do not render this as success.
		writeError(w, http.StatusConflict, err)
		return
	}
	if created.Status == release.ReleaseSucceeded {
		writeJSON(w, http.StatusOK, created)
		return
	}
	writeJSON(w, http.StatusMultiStatus, created)
}
