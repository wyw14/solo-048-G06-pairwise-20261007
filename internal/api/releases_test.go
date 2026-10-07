package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func callAPI(t *testing.T, handler http.Handler, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	} else {
		reader = bytes.NewReader(nil)
	}
	request := httptest.NewRequest(method, path, reader)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var envelope map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode response: %v body=%s", err, response.Body.String())
	}
	return response.Code, envelope
}

func draftReleaseApprovePublish(t *testing.T, app *App, targets map[string]float64) (int, map[string]any) {
	t.Helper()
	handler := app.Handler()
	status, envelope := callAPI(t, handler, http.MethodPost, "/api/v1/target-drafts", map[string]any{"targets": targets, "actor": "planner"})
	if status != http.StatusCreated {
		t.Fatalf("draft status = %d envelope=%v", status, envelope)
	}
	draft := envelope["data"].(map[string]any)
	status, envelope = callAPI(t, handler, http.MethodPost, "/api/v1/releases", map[string]any{"draft_id": draft["id"], "actor": "planner"})
	if status != http.StatusCreated {
		t.Fatalf("release status = %d envelope=%v", status, envelope)
	}
	created := envelope["data"].(map[string]any)
	for _, zone := range created["zones"].([]any) {
		zoneID := zone.(map[string]any)["zone_id"].(string)
		path := "/api/v1/releases/" + created["id"].(string) + "/zones/" + zoneID + "/approve"
		if status, envelope = callAPI(t, handler, http.MethodPost, path, map[string]any{"actor": "lead"}); status != http.StatusOK {
			t.Fatalf("approve %s status = %d envelope=%v", zoneID, status, envelope)
		}
	}
	return callAPI(t, handler, http.MethodPost, "/api/v1/releases/"+created["id"].(string)+"/publish", map[string]any{"actor": "dispatcher"})
}

func TestReleasePublishEndpointSucceeds(t *testing.T) {
	app, err := NewApp(t.TempDir())
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	status, envelope := draftReleaseApprovePublish(t, app, map[string]float64{"xincheng": 70, "binhe": 67, "nanhu": 65, "xijiao": 66})
	if status != http.StatusOK {
		t.Fatalf("publish status = %d envelope=%v", status, envelope)
	}
	created := envelope["data"].(map[string]any)
	if created["status"] != "succeeded" {
		t.Fatalf("status = %v, want succeeded", created["status"])
	}
}

func TestReleasePublishEndpointReturnsMultiStatusOnPartialFailure(t *testing.T) {
	app, err := NewApp(t.TempDir())
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	// xijiao (95 envelope) must fail; the other three controllers succeed.
	status, envelope := draftReleaseApprovePublish(t, app, map[string]float64{"xincheng": 70, "binhe": 67, "nanhu": 65, "xijiao": 200})
	if status != http.StatusMultiStatus {
		t.Fatalf("publish status = %d, want 207; envelope=%v", status, envelope)
	}
	created := envelope["data"].(map[string]any)
	if created["status"] != "partial_failure" {
		t.Fatalf("status = %v, want partial_failure", created["status"])
	}
	results := created["results"].([]any)
	failures := 0
	for _, raw := range results {
		result := raw.(map[string]any)
		if result["acknowledged"] == false {
			failures++
			if result["station_id"] != "xijiao" || result["error"] == "" {
				t.Fatalf("unexpected failure result: %v", result)
			}
		}
	}
	if failures != 1 {
		t.Fatalf("failures = %d, want 1", failures)
	}
}

func TestReleasePublishRejectedWithoutApproval(t *testing.T) {
	app, err := NewApp(t.TempDir())
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	handler := app.Handler()
	_, envelope := callAPI(t, handler, http.MethodPost, "/api/v1/target-drafts", map[string]any{
		"targets": map[string]float64{"xincheng": 70}, "actor": "planner",
	})
	draft := envelope["data"].(map[string]any)
	_, envelope = callAPI(t, handler, http.MethodPost, "/api/v1/releases", map[string]any{"draft_id": draft["id"], "actor": "planner"})
	created := envelope["data"].(map[string]any)
	status, envelope := callAPI(t, handler, http.MethodPost, "/api/v1/releases/"+created["id"].(string)+"/publish", map[string]any{"actor": "dispatcher"})
	if status != http.StatusConflict {
		t.Fatalf("publish status = %d, want 409; envelope=%v", status, envelope)
	}
}
