package release

import (
	"path/filepath"
	"testing"
	"time"

	"heatpulse/internal/audit"
	"heatpulse/internal/station"
	"heatpulse/internal/store"
)

func newTestService(t *testing.T) *Service {
	t.Helper()
	stations := station.NewRegistry([]station.Station{
		{ID: "xincheng", Name: "新城站", ZoneID: "north", Online: true, TargetCelsius: 68, TargetRevision: 1, ControllerEpoch: 1},
		{ID: "binhe", Name: "滨河站", ZoneID: "east", Online: true, TargetCelsius: 66, TargetRevision: 1, ControllerEpoch: 1},
		{ID: "nanhu", Name: "南湖站", ZoneID: "west", Online: true, TargetCelsius: 64, TargetRevision: 1, ControllerEpoch: 1},
		{ID: "xijiao", Name: "西郊站", ZoneID: "west", Online: false, TargetCelsius: 65, TargetRevision: 1, ControllerEpoch: 1},
	})
	targets := station.NewTargetService(stations)
	commands := station.NewCommandService(stations)
	events := store.NewEventLog(filepath.Join(t.TempDir(), "events.jsonl"))
	return NewService(stations, targets, commands, audit.NewRecorder(events))
}

func approveAll(t *testing.T, svc *Service, draft Draft) Draft {
	t.Helper()
	current := draft
	for _, zoneID := range current.Zones() {
		var err error
		current, err = svc.ApproveZone(current.ID, zoneID, "dispatcher")
		if err != nil {
			t.Fatalf("approve zone %s: %v", zoneID, err)
		}
	}
	return current
}

// acknowledgePending confirms every still-pending controller at the epoch the
// command was sent with.
func acknowledgePending(t *testing.T, svc *Service, releaseID string, r Release) Release {
	t.Helper()
	current := r
	for _, result := range current.Results {
		if result.Status != ResultPending {
			continue
		}
		var err error
		current, err = svc.Acknowledge(releaseID, result.StationID, result.RequestedEpoch)
		if err != nil {
			t.Fatalf("acknowledge %s: %v", result.StationID, err)
		}
	}
	return current
}

func TestGenerateDraftComparesCurrentSettings(t *testing.T) {
	svc := newTestService(t)
	draft, err := svc.GenerateDraft(map[string]float64{"xincheng": 70, "binhe": 66})
	if err != nil {
		t.Fatalf("generate draft: %v", err)
	}
	if draft.Status != StatusDraft {
		t.Fatalf("draft status = %s, want draft", draft.Status)
	}
	byStation := map[string]StationChange{}
	for _, change := range draft.Changes {
		byStation[change.StationID] = change
	}
	xincheng := byStation["xincheng"]
	if !xincheng.Changed || xincheng.Delta != 2 || xincheng.Current != 68 || xincheng.Proposed != 70 {
		t.Fatalf("xincheng comparison = %+v", xincheng)
	}
	if xincheng.CurrentEpoch != 1 {
		t.Fatalf("xincheng epoch = %d, want 1", xincheng.CurrentEpoch)
	}
	binhe := byStation["binhe"]
	if binhe.Changed || binhe.Note != "no temperature change" {
		t.Fatalf("binhe should be unchanged, got %+v", binhe)
	}
	if got := draft.Zones(); len(got) != 2 || got[0] != "east" || got[1] != "north" {
		t.Fatalf("zones = %v", got)
	}
}

func TestGenerateDraftRejectsUnknownStationAndEnvelope(t *testing.T) {
	svc := newTestService(t)
	if _, err := svc.GenerateDraft(map[string]float64{"ghost": 70}); err == nil {
		t.Fatal("expected error for unknown station")
	}
	if _, err := svc.GenerateDraft(map[string]float64{"xincheng": 200}); err == nil {
		t.Fatal("expected error for out-of-envelope target")
	}
}

func TestPublishRequiresEveryZoneApproval(t *testing.T) {
	svc := newTestService(t)
	draft, _ := svc.GenerateDraft(map[string]float64{"xincheng": 70, "binhe": 68})
	if _, err := svc.Publish(draft.ID, "dispatcher"); err == nil {
		t.Fatal("publish without approvals must fail")
	}
	if _, err := svc.ApproveZone(draft.ID, "north", "dispatcher"); err != nil {
		t.Fatalf("approve north: %v", err)
	}
	if _, err := svc.Publish(draft.ID, "dispatcher"); err == nil {
		t.Fatal("publish with only north approved must fail: east still pending")
	}
}

func TestApproveUnknownZoneRejected(t *testing.T) {
	svc := newTestService(t)
	draft, _ := svc.GenerateDraft(map[string]float64{"xincheng": 70})
	if _, err := svc.ApproveZone(draft.ID, "west", "dispatcher"); err == nil {
		t.Fatal("approving a zone the draft does not touch must fail")
	}
}

func TestPublishTracksEpochAndReceiptPerController(t *testing.T) {
	svc := newTestService(t)
	draft, _ := svc.GenerateDraft(map[string]float64{"xincheng": 70, "binhe": 68, "nanhu": 66})
	draft = approveAll(t, svc, draft)
	if draft.Status != StatusApproved {
		t.Fatalf("draft status = %s, want approved", draft.Status)
	}

	release, err := svc.Publish(draft.ID, "dispatcher")
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if release.Status != StatusPublishing {
		t.Fatalf("release status = %s, want publishing", release.Status)
	}
	result := release.Results["xincheng"]
	if result.RequestedEpoch != 2 || result.CommandID == "" || result.Status != ResultPending {
		t.Fatalf("xincheng dispatch = %+v", result)
	}

	// A receipt carrying an older epoch means the controller failed over; it
	// must be tracked as a stale-epoch failure, not an acknowledgement.
	stale, err := svc.Acknowledge(release.ID, "binhe", 1)
	if err != nil {
		t.Fatalf("stale receipt: %v", err)
	}
	if got := stale.Results["binhe"].Status; got != ResultStaleEpoch {
		t.Fatalf("binhe status = %s, want stale_epoch", got)
	}
	// The pending xincheng command acks at the requested epoch.
	release, err = svc.Acknowledge(release.ID, "xincheng", 2)
	if err != nil {
		t.Fatalf("ack xincheng: %v", err)
	}
	if got := release.Results["xincheng"]; got.Status != ResultAcknowledged || got.AppliedEpoch != 2 || got.AckedAt.IsZero() {
		t.Fatalf("xincheng receipt = %+v", got)
	}
	if _, err := svc.Acknowledge(release.ID, "xincheng", 2); err == nil {
		t.Fatal("double acknowledgement must fail")
	}
}

func TestPartialFailureIsNeverReportedAsSuccess(t *testing.T) {
	svc := newTestService(t)
	// xijiao is offline and nanhu is online in the same west zone.
	draft, _ := svc.GenerateDraft(map[string]float64{"xincheng": 70, "binhe": 68, "nanhu": 66, "xijiao": 67})
	draft = approveAll(t, svc, draft)
	release, err := svc.Publish(draft.ID, "dispatcher")
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if got := release.Results["xijiao"].Status; got != ResultOffline {
		t.Fatalf("xijiao status = %s, want offline", got)
	}
	success, pending, failed := release.Counts()
	if success != 0 || pending != 3 || failed != 1 {
		t.Fatalf("counts = success %d pending %d failed %d", success, pending, failed)
	}

	release = acknowledgePending(t, svc, release.ID, release)
	if release.Status != StatusPartialFail {
		t.Fatalf("release status = %s, want partial_fail", release.Status)
	}
	if release.Status == StatusSucceeded {
		t.Fatal("a batch with a failed controller must never be succeeded")
	}
	success, pending, failed = release.Counts()
	if success != 3 || pending != 0 || failed != 1 {
		t.Fatalf("final counts = success %d pending %d failed %d", success, pending, failed)
	}

	// The originating draft mirrors the partial-failure status.
	storedDraft, ok := svc.Draft(draft.ID)
	if !ok || storedDraft.Status != StatusPartialFail {
		t.Fatalf("draft status = %s, want partial_fail", storedDraft.Status)
	}
	// The draft cannot be published a second time.
	if _, err := svc.Publish(draft.ID, "dispatcher"); err == nil {
		t.Fatal("republishing a released draft must fail")
	}
}

func TestAllReceiptsSucceedsBatch(t *testing.T) {
	svc := newTestService(t)
	draft, _ := svc.GenerateDraft(map[string]float64{"xincheng": 70, "binhe": 68})
	draft = approveAll(t, svc, draft)
	release, err := svc.Publish(draft.ID, "dispatcher")
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	release = acknowledgePending(t, svc, release.ID, release)
	if release.Status != StatusSucceeded {
		t.Fatalf("release status = %s, want succeeded", release.Status)
	}
	for stationID, result := range release.Results {
		if !result.OK() {
			t.Fatalf("%s not acknowledged: %s", stationID, result.Status)
		}
	}
}

func TestPublishRejectsStaleDraft(t *testing.T) {
	svc := newTestService(t)
	draft, _ := svc.GenerateDraft(map[string]float64{"xincheng": 70})
	draft = approveAll(t, svc, draft)

	// Live target revision advances after the draft was generated.
	if _, err := svc.targets.ManualHold([]string{"binhe"}, "operator"); err != nil {
		t.Fatalf("advance revision: %v", err)
	}
	if _, err := svc.Publish(draft.ID, "dispatcher"); err == nil {
		t.Fatal("publishing against an advanced revision must fail")
	}
	stored, ok := svc.Draft(draft.ID)
	if !ok || stored.Status != StatusStale {
		t.Fatalf("draft status = %s, want stale", stored.Status)
	}
}

func TestReconcileExpiresMissingReceipts(t *testing.T) {
	svc := newTestService(t)
	previous := receiptWindow
	receiptWindow = time.Millisecond
	defer func() { receiptWindow = previous }()

	draft, _ := svc.GenerateDraft(map[string]float64{"xincheng": 70, "binhe": 68})
	draft = approveAll(t, svc, draft)
	release, err := svc.Publish(draft.ID, "dispatcher")
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	release, err = svc.Reconcile(release.ID)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if release.Status != StatusFailed {
		t.Fatalf("release status = %s, want failed after both receipts lapse", release.Status)
	}
	for stationID, result := range release.Results {
		if result.Status != ResultNoReceipt {
			t.Fatalf("%s status = %s, want no_receipt", stationID, result.Status)
		}
	}
}
