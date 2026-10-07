package release

import (
	"path/filepath"
	"testing"

	"heatpulse/internal/audit"
	"heatpulse/internal/network"
	"heatpulse/internal/station"
	"heatpulse/internal/store"
)

func newFixture(t *testing.T) (*Service, *station.Registry, *station.TargetService) {
	t.Helper()
	zones := network.NewState([]network.Zone{
		{ID: "north", Name: "北环", Phase: network.PhaseActive},
		{ID: "west", Name: "西区", Phase: network.PhaseActive},
	})
	registry := station.NewRegistry([]station.Station{
		{ID: "xincheng", Name: "新城站", ZoneID: "north", Online: true, TargetCelsius: 68, TargetRevision: 1, ControllerEpoch: 1},
		{ID: "nanhu", Name: "南湖站", ZoneID: "west", Online: true, TargetCelsius: 64, TargetRevision: 1, ControllerEpoch: 1},
	})
	targets := station.NewTargetService(registry)
	commands := station.NewCommandService(registry)
	recorder := audit.NewRecorder(store.NewEventLog(filepath.Join(t.TempDir(), "events.jsonl")))
	service := NewService(registry, zones, targets, commands, recorder)
	return service, registry, targets
}

func approveAll(t *testing.T, service *Service, created Release) {
	t.Helper()
	for _, zone := range created.Zones {
		if _, err := service.ApproveZone(created.ID, zone.ZoneID, "dispatcher"); err != nil {
			t.Fatalf("approve zone %s: %v", zone.ZoneID, err)
		}
	}
}

func TestDraftComparesCurrentTargets(t *testing.T) {
	service, _, _ := newFixture(t)
	draft, err := service.CreateDraft(map[string]float64{"xincheng": 70, "nanhu": 64}, "planner", "")
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}
	if draft.SourceRevision != 1 {
		t.Fatalf("source revision = %d, want 1", draft.SourceRevision)
	}
	changed := map[string]bool{}
	issues := map[string]string{}
	for _, entry := range draft.Entries {
		changed[entry.StationID] = entry.Changed
		issues[entry.StationID] = entry.Issue
		if entry.ControllerEpoch != 1 {
			t.Fatalf("entry %s epoch = %d, want 1", entry.StationID, entry.ControllerEpoch)
		}
	}
	if !changed["xincheng"] || changed["nanhu"] {
		t.Fatalf("change flags = %v, want xincheng=true nanhu=false", changed)
	}
	if issues["xincheng"] != "" || issues["nanhu"] != "" {
		t.Fatalf("unexpected draft issues: %v", issues)
	}
}

func TestDraftFlagsOutOfEnvelopeTarget(t *testing.T) {
	service, _, _ := newFixture(t)
	draft, err := service.CreateDraft(map[string]float64{"xincheng": 200}, "planner", "")
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}
	if draft.Entries[0].Issue == "" {
		t.Fatal("expected out-of-envelope issue on draft entry")
	}
}

func TestPublishSucceedsWithReceiptsForEveryController(t *testing.T) {
	service, registry, _ := newFixture(t)
	draft, err := service.CreateDraft(map[string]float64{"xincheng": 70, "nanhu": 66}, "planner", "")
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}
	created, err := service.CreateRelease(draft.ID, "planner", "")
	if err != nil {
		t.Fatalf("create release: %v", err)
	}
	if created.AllZonesApproved() {
		t.Fatal("fresh release must not be pre-approved")
	}
	if _, err := service.Publish(created.ID, "dispatcher"); err == nil {
		t.Fatal("publish before approvals must fail")
	}
	approveAll(t, service, created)
	published, err := service.Publish(created.ID, "dispatcher")
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if published.Status != ReleaseSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", published.Status, published.Message)
	}
	counts := published.Counts()
	if counts.Applied != 2 || counts.Failed != 0 {
		t.Fatalf("counts = %+v, want 2 applied / 0 failed", counts)
	}
	for _, result := range published.Results {
		if !result.Acknowledged || result.ReceiptStatus != "applied" {
			t.Fatalf("result %+v not acknowledged", result)
		}
		if result.Epoch != 1 || result.Sequence != 1 || result.CommandID == "" {
			t.Fatalf("result %+v missing epoch/sequence/command id", result)
		}
	}
	item, _ := registry.Get("xincheng")
	if item.TargetCelsius != 70 || item.TargetRevision != 2 {
		t.Fatalf("xincheng target = %.1f rev %d, want 70 rev 2", item.TargetCelsius, item.TargetRevision)
	}
}

func TestPublishPartialFailureIsNotReportedAsSuccess(t *testing.T) {
	service, _, _ := newFixture(t)
	// nanhu target is outside the operating envelope: target update and
	// command for that controller must fail while xincheng goes through.
	draft, err := service.CreateDraft(map[string]float64{"xincheng": 70, "nanhu": 200}, "planner", "")
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}
	created, err := service.CreateRelease(draft.ID, "planner", "")
	if err != nil {
		t.Fatalf("create release: %v", err)
	}
	approveAll(t, service, created)
	published, err := service.Publish(created.ID, "dispatcher")
	if err != nil {
		t.Fatalf("partial publish should be returned with the aggregate, got %v", err)
	}
	if published.Status != ReleasePartialFailure {
		t.Fatalf("status = %s, want partial_failure", published.Status)
	}
	counts := published.Counts()
	if counts.Applied != 1 || counts.Failed != 1 {
		t.Fatalf("counts = %+v, want 1 applied / 1 failed", counts)
	}
	byStation := map[string]ControllerResult{}
	for _, result := range published.Results {
		byStation[result.StationID] = result
	}
	if !byStation["xincheng"].Acknowledged {
		t.Fatal("xincheng should have been acknowledged")
	}
	failed := byStation["nanhu"]
	if failed.Acknowledged || failed.Error == "" || failed.CommandID != "" {
		t.Fatalf("nanhu result = %+v, want failure with error and no command", failed)
	}
}

func TestPublishOfflineControllerFailsReceipt(t *testing.T) {
	service, registry, _ := newFixture(t)
	if _, err := registry.SetOnline("nanhu", false); err != nil {
		t.Fatalf("set offline: %v", err)
	}
	draft, err := service.CreateDraft(map[string]float64{"xincheng": 70, "nanhu": 66}, "planner", "")
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}
	created, err := service.CreateRelease(draft.ID, "planner", "")
	if err != nil {
		t.Fatalf("create release: %v", err)
	}
	approveAll(t, service, created)
	published, err := service.Publish(created.ID, "dispatcher")
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if published.Status != ReleasePartialFailure {
		t.Fatalf("status = %s, want partial_failure", published.Status)
	}
	for _, result := range published.Results {
		if result.StationID == "nanhu" && (result.Acknowledged || result.TargetApplied != true) {
			t.Fatalf("offline nanhu result = %+v, want applied target but no receipt", result)
		}
	}
}

func TestRejectedZoneBlocksPublish(t *testing.T) {
	service, _, _ := newFixture(t)
	draft, _ := service.CreateDraft(map[string]float64{"xincheng": 70, "nanhu": 66}, "planner", "")
	created, _ := service.CreateRelease(draft.ID, "planner", "")
	rejected, err := service.RejectZone(created.ID, "west", "lead", "pump work ongoing")
	if err != nil {
		t.Fatalf("reject zone: %v", err)
	}
	if rejected.Status != ReleaseRejected {
		t.Fatalf("status = %s, want rejected", rejected.Status)
	}
	if _, err := service.ApproveZone(created.ID, "west", "lead"); err == nil {
		t.Fatal("approving a rejected release must fail")
	}
	if _, err := service.Publish(created.ID, "dispatcher"); err == nil {
		t.Fatal("publish of a zone-rejected release must fail")
	}
}

func TestPublishExpiresWhenRevisionMoved(t *testing.T) {
	service, _, targets := newFixture(t)
	draft, _ := service.CreateDraft(map[string]float64{"xincheng": 70, "nanhu": 66}, "planner", "")
	created, _ := service.CreateRelease(draft.ID, "planner", "")
	approveAll(t, service, created)
	// Concurrent dispatch bumps the revision before this release publishes.
	if err := targets.CompareAndPublish(draft.SourceRevision, map[string]float64{"xincheng": 71}); err != nil {
		t.Fatalf("concurrent publish: %v", err)
	}
	published, err := service.Publish(created.ID, "dispatcher")
	if err == nil {
		t.Fatal("stale publish must error")
	}
	if published.Status != ReleaseExpired {
		t.Fatalf("status = %s, want expired", published.Status)
	}
	for _, result := range published.Results {
		if result.CommandID != "" || result.Acknowledged {
			t.Fatalf("stale result %+v must not carry a command receipt", result)
		}
	}
}

func TestSecondBatchContinuesSequence(t *testing.T) {
	service, _, _ := newFixture(t)
	runBatch := func(targets map[string]float64) {
		draft, err := service.CreateDraft(targets, "planner", "")
		if err != nil {
			t.Fatalf("create draft: %v", err)
		}
		created, err := service.CreateRelease(draft.ID, "planner", "")
		if err != nil {
			t.Fatalf("create release: %v", err)
		}
		approveAll(t, service, created)
		if _, err := service.Publish(created.ID, "dispatcher"); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}
	runBatch(map[string]float64{"xincheng": 70, "nanhu": 66})
	runBatch(map[string]float64{"xincheng": 72, "nanhu": 67})
	releases := service.Releases() // newest first
	if len(releases) != 2 {
		t.Fatalf("releases = %d, want 2", len(releases))
	}
	second, first := releases[0], releases[1]
	for _, result := range second.Results {
		if result.Sequence != 2 {
			t.Fatalf("second batch %s sequence = %d, want 2", result.StationID, result.Sequence)
		}
	}
	for _, result := range first.Results {
		if result.Sequence != 1 {
			t.Fatalf("first batch %s sequence = %d, want 1", result.StationID, result.Sequence)
		}
	}
}
