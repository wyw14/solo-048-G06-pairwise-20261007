// Package release manages site target-version releases: a draft is generated
// from proposed targets and compared against the live temperature settings,
// each affected zone approves independently, and only then is the batch
// published as controller commands. Every controller's epoch, command and
// receipt is tracked individually so that a partial failure can never be
// reported as a successful batch.
package release

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"heatpulse/internal/audit"
	"heatpulse/internal/station"
)

// receiptWindow is how long a published batch waits for controller
// acknowledgements before still-pending controllers count as missing receipts.
// It is a variable so tests can shrink the window without waiting.
var receiptWindow = 30 * time.Second

// Service owns the drafts and release batches for a site.
type Service struct {
	registry *station.Registry
	targets  *station.TargetService
	commands *station.CommandService
	audit    *audit.Recorder

	mu       sync.RWMutex
	drafts   map[string]Draft
	releases map[string]Release
}

func NewService(registry *station.Registry, targets *station.TargetService, commands *station.CommandService, recorder *audit.Recorder) *Service {
	return &Service{
		registry: registry, targets: targets, commands: commands, audit: recorder,
		drafts: make(map[string]Draft), releases: make(map[string]Release),
	}
}

// GenerateDraft builds a proposal from requested targets and snapshots the
// current settings of every affected station for comparison. An empty request
// drafts every station so operators can review the full site baseline.
func (s *Service) GenerateDraft(requested map[string]float64) (Draft, error) {
	now := time.Now().UTC()
	items := s.registry.List()

	pick := requested
	draftAll := len(requested) == 0
	if draftAll {
		pick = make(map[string]float64, len(items))
		for _, item := range items {
			pick[item.ID] = item.TargetCelsius
		}
	}
	if len(pick) == 0 {
		return Draft{}, fmt.Errorf("at least one station target is required")
	}

	changes := make([]StationChange, 0, len(pick))
	for _, item := range items {
		proposed, requested := pick[item.ID]
		if !requested {
			continue
		}
		if proposed < station.MinTargetCelsius || proposed > station.MaxTargetCelsius {
			return Draft{}, fmt.Errorf("target %.1f for %s outside operating envelope [%.0f, %.0f]",
				proposed, item.ID, station.MinTargetCelsius, station.MaxTargetCelsius)
		}
		change := StationChange{
			StationID: item.ID, Name: item.Name, ZoneID: item.ZoneID,
			Current: item.TargetCelsius, Proposed: proposed, Delta: proposed - item.TargetCelsius,
			Changed: !temperatureEqual(proposed, item.TargetCelsius),
			Online:  item.Online, OnHold: item.Hold, InMaintenance: item.Maintenance,
			CurrentEpoch: item.ControllerEpoch,
		}
		switch {
		case change.InMaintenance:
			change.Note = "station in maintenance"
		case change.OnHold:
			change.Note = "station manually held"
		case !change.Online:
			change.Note = "station offline"
		case !change.Changed:
			change.Note = "no temperature change"
		}
		changes = append(changes, change)
	}

	known := make(map[string]struct{}, len(items))
	for _, item := range items {
		known[item.ID] = struct{}{}
	}
	for stationID := range requested {
		if _, ok := known[stationID]; !ok {
			return Draft{}, fmt.Errorf("station %s not found", stationID)
		}
	}

	draft := Draft{
		ID: uuid.NewString(), SourceRevision: s.targets.Revision(), Status: StatusDraft,
		Changes: changes, Approvals: make(map[string]ZoneApproval),
		CreatedAt: now, UpdatedAt: now,
	}
	s.mu.Lock()
	s.drafts[draft.ID] = draft.Clone()
	s.mu.Unlock()
	return draft.Clone(), nil
}

// Draft returns a stored draft.
func (s *Service) Draft(draftID string) (Draft, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	draft, ok := s.drafts[draftID]
	return draft.Clone(), ok
}

// Drafts returns all drafts, newest first.
func (s *Service) Drafts() []Draft {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]Draft, 0, len(s.drafts))
	for _, draft := range s.drafts {
		result = append(result, draft.Clone())
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.After(result[j].CreatedAt) })
	return result
}

// ApproveZone records a zone's sign-off. All affected zones must approve
// before the draft can be published.
func (s *Service) ApproveZone(draftID, zoneID, actor string) (Draft, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	draft, ok := s.drafts[draftID]
	if !ok {
		return Draft{}, fmt.Errorf("draft %s not found", draftID)
	}
	if draft.Status != StatusDraft {
		return draft.Clone(), fmt.Errorf("draft %s is %s and cannot be approved", draftID, draft.Status)
	}
	touchesZone := false
	for _, change := range draft.Changes {
		if change.ZoneID == zoneID {
			touchesZone = true
			break
		}
	}
	if !touchesZone {
		return draft.Clone(), fmt.Errorf("draft %s does not affect zone %s", draftID, zoneID)
	}
	draft.Approvals[zoneID] = ZoneApproval{ZoneID: zoneID, Approved: true, ApprovedBy: actor, ApprovedAt: time.Now().UTC()}
	draft.UpdatedAt = time.Now().UTC()
	if draft.Approved() {
		draft.Status = StatusApproved
	}
	s.drafts[draftID] = draft.Clone()
	return draft.Clone(), nil
}

// Publish sends the approved draft to every targeted controller. The batch is
// created even when individual stations cannot be reached; those stations are
// recorded as failures and the batch only reaches "succeeded" once every
// remaining controller returns a receipt.
func (s *Service) Publish(draftID, actor string) (Release, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	draft, ok := s.drafts[draftID]
	if !ok {
		return Release{}, fmt.Errorf("draft %s not found", draftID)
	}
	switch {
	case draft.Status == StatusStale:
		return Release{}, fmt.Errorf("draft %s is stale; regenerate before publishing", draftID)
	case draft.ReleaseID != "":
		return Release{}, fmt.Errorf("draft %s already published as %s", draftID, draft.ReleaseID)
	case !draft.Approved():
		return Release{}, fmt.Errorf("draft %s awaits zone approval: %v", draftID, missingApprovals(draft))
	}

	// Optimistic concurrency: live targets must not have moved since the draft.
	if current := s.targets.Revision(); current != draft.SourceRevision {
		draft.Status = StatusStale
		draft.Message = fmt.Sprintf("target revision advanced from %d to %d", draft.SourceRevision, current)
		draft.UpdatedAt = time.Now().UTC()
		s.drafts[draftID] = draft.Clone()
		return Release{}, fmt.Errorf("draft %s is stale: target revision is now %d (was %d)", draftID, current, draft.SourceRevision)
	}

	now := time.Now().UTC()
	release := Release{
		ID: uuid.NewString(), DraftID: draftID, Status: StatusPublishing,
		Results: make(map[string]StationResult), CreatedAt: now, UpdatedAt: now, PublishedBy: actor,
	}

	for _, change := range draft.Changes {
		result := StationResult{
			StationID: change.StationID, ZoneID: change.ZoneID, Sequence: 1,
			RequestedEpoch: change.CurrentEpoch + 1, TargetCelsius: change.Proposed,
		}
		item, exists := s.registry.Get(change.StationID)
		if !exists {
			result.Status = ResultRejected
			result.Detail = "station vanished before publish"
			release.Results[change.StationID] = result
			continue
		}
		switch {
		case item.Maintenance:
			result.Status = ResultRejected
			result.Detail = "station in maintenance"
			release.Results[change.StationID] = result
			continue
		case item.Hold:
			result.Status = ResultRejected
			result.Detail = "station manually held"
			release.Results[change.StationID] = result
			continue
		case !item.Online:
			result.Status = ResultOffline
			result.Detail = "controller unreachable"
			release.Results[change.StationID] = result
			continue
		}

		// The epoch fencing ledger must see a strictly newer epoch.
		if last, ok := s.commands.Last(change.StationID); ok && last.Epoch >= result.RequestedEpoch {
			result.RequestedEpoch = last.Epoch + 1
		}

		if _, err := s.targets.ApplyTarget(change.StationID, change.Proposed); err != nil {
			result.Status = ResultRejected
			result.Detail = err.Error()
			release.Results[change.StationID] = result
			continue
		}
		command, err := s.commands.Apply(station.Command{
			ID: uuid.NewString(), StationID: change.StationID, Epoch: result.RequestedEpoch,
			Sequence: result.Sequence, Celsius: change.Proposed, At: now,
		})
		if err != nil {
			result.Status = ResultRejected
			result.Detail = err.Error()
			release.Results[change.StationID] = result
			continue
		}
		result.CommandID = command.ID
		result.Status = ResultPending
		result.SentAt = now
		release.Results[change.StationID] = result
	}

	release.Status = classify(release)
	s.releases[release.ID] = release.Clone()
	draft.Status = release.Status
	draft.ReleaseID = release.ID
	draft.UpdatedAt = now
	s.drafts[draftID] = draft.Clone()

	sent, failed := 0, 0
	for _, result := range release.Results {
		switch result.Status {
		case ResultPending:
			sent++
		default:
			failed++
		}
	}
	outcome := audit.OutcomeAccepted
	if release.Status == StatusFailed {
		outcome = audit.OutcomeFailed
	}
	_, _ = s.audit.Record(audit.Entry{
		Action: "release.publish", EntityID: release.ID, Actor: actor, Outcome: outcome,
		Message: fmt.Sprintf("release dispatched: %d commands sent, %d stations failed", sent, failed),
		Details: map[string]any{"draft_id": draftID, "sent": sent, "failed": failed},
	})
	return release.Clone(), nil
}

// Acknowledge records a controller receipt for a pending command. The reported
// applied epoch must match the epoch the command was sent with; a controller
// that failed over mid-flight is recorded as a stale-epoch failure.
func (s *Service) Acknowledge(releaseID, stationID string, appliedEpoch uint64) (Release, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	release, ok := s.releases[releaseID]
	if !ok {
		return Release{}, fmt.Errorf("release %s not found", releaseID)
	}
	result, ok := release.Results[stationID]
	if !ok {
		return release.Clone(), fmt.Errorf("release %s did not target station %s", releaseID, stationID)
	}
	if result.Status != ResultPending {
		return release.Clone(), fmt.Errorf("station %s already resolved as %s", stationID, result.Status)
	}
	if last, ok := s.commands.Last(stationID); !ok || last.ID != result.CommandID {
		result.Status = ResultRejected
		result.Detail = "receipt does not match the dispatched command"
	} else if appliedEpoch < result.RequestedEpoch {
		result.Status = ResultStaleEpoch
		result.Detail = fmt.Sprintf("receipt epoch %d older than requested %d", appliedEpoch, result.RequestedEpoch)
	} else {
		result.Status = ResultAcknowledged
		result.AppliedEpoch = appliedEpoch
		result.AckedAt = time.Now().UTC()
	}
	release.Results[stationID] = result
	release.UpdatedAt = time.Now().UTC()
	release.Status = classify(release)
	s.releases[releaseID] = release.Clone()
	s.syncDraftStatus(release)
	if release.Status.Terminal() {
		s.recordCompletion(release)
	}
	return release.Clone(), nil
}

// Reconcile expires commands whose receipt window has elapsed without an
// acknowledgement and recomputes the batch status.
func (s *Service) Reconcile(releaseID string) (Release, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	release, ok := s.releases[releaseID]
	if !ok {
		return Release{}, fmt.Errorf("release %s not found", releaseID)
	}
	cutoff := time.Now().UTC().Add(-receiptWindow)
	changed := false
	for stationID, result := range release.Results {
		if result.Status == ResultPending && !result.SentAt.After(cutoff) {
			result.Status = ResultNoReceipt
			result.Detail = "no controller receipt within window"
			release.Results[stationID] = result
			changed = true
		}
	}
	if changed {
		release.UpdatedAt = time.Now().UTC()
		release.Status = classify(release)
		s.releases[releaseID] = release.Clone()
		s.syncDraftStatus(release)
		if release.Status.Terminal() {
			s.recordCompletion(release)
		}
	}
	return release.Clone(), nil
}

// Release returns a stored release batch.
func (s *Service) Release(releaseID string) (Release, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	release, ok := s.releases[releaseID]
	return release.Clone(), ok
}

// Releases returns all batches, newest first.
func (s *Service) Releases() []Release {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]Release, 0, len(s.releases))
	for _, release := range s.releases {
		result = append(result, release.Clone())
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.After(result[j].CreatedAt) })
	return result
}

// syncDraftStatus keeps the originating draft aligned with its batch. Caller
// must hold s.mu.
func (s *Service) syncDraftStatus(release Release) {
	draft, ok := s.drafts[release.DraftID]
	if !ok {
		return
	}
	draft.Status = release.Status
	draft.UpdatedAt = release.UpdatedAt
	s.drafts[release.DraftID] = draft.Clone()
}

// recordCompletion writes the terminal audit outcome. Caller holds s.mu.
func (s *Service) recordCompletion(release Release) {
	success, pending, failed := release.Counts()
	outcome := audit.OutcomeCompleted
	if release.Status == StatusPartialFail || release.Status == StatusFailed {
		outcome = audit.OutcomeFailed
	}
	_, _ = s.audit.Record(audit.Entry{
		Action: "release.complete", EntityID: release.ID, Actor: release.PublishedBy, Outcome: outcome,
		Message: fmt.Sprintf("release %s: %d acknowledged, %d pending, %d failed", release.Status, success, pending, failed),
		Details: map[string]any{"status": release.Status, "acknowledged": success, "pending": pending, "failed": failed},
	})
}

// classify derives the batch status from per-controller results. The batch is
// only ever "succeeded" when zero controllers are pending or failed.
func classify(release Release) Status {
	success, pending, failed := release.Counts()
	switch {
	case pending > 0:
		return StatusPublishing
	case failed > 0 && success > 0:
		return StatusPartialFail
	case failed > 0:
		return StatusFailed
	default:
		return StatusSucceeded
	}
}

func missingApprovals(draft Draft) []string {
	missing := make([]string, 0)
	for _, zoneID := range draft.Zones() {
		if !draft.Approvals[zoneID].Approved {
			missing = append(missing, zoneID)
		}
	}
	return missing
}
