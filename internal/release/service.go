package release

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"heatpulse/internal/audit"
	"heatpulse/internal/network"
	"heatpulse/internal/station"
)

type Service struct {
	stations *station.Registry
	zones    *network.State
	targets  *station.TargetService
	commands *station.CommandService
	audit    *audit.Recorder

	mu        sync.Mutex
	drafts    map[string]Draft
	releases  map[string]Release
	sequences map[string]uint64
}

func NewService(
	stations *station.Registry,
	zones *network.State,
	targets *station.TargetService,
	commands *station.CommandService,
	recorder *audit.Recorder,
) *Service {
	return &Service{
		stations:  stations,
		zones:     zones,
		targets:   targets,
		commands:  commands,
		audit:     recorder,
		drafts:    make(map[string]Draft),
		releases:  make(map[string]Release),
		sequences: make(map[string]uint64),
	}
}

// CreateDraft compares the desired target for every named station against
// the currently running target and pins the dispatch revision for a later
// publish.
func (s *Service) CreateDraft(desired map[string]float64, actor, note string) (Draft, error) {
	if len(desired) == 0 {
		return Draft{}, fmt.Errorf("at least one station target is required")
	}
	stationIDs := make([]string, 0, len(desired))
	for stationID := range desired {
		stationIDs = append(stationIDs, stationID)
	}
	sort.Strings(stationIDs)
	entries := make([]DraftEntry, 0, len(stationIDs))
	zoneSet := make(map[string]struct{})
	for _, stationID := range stationIDs {
		item, ok := s.stations.Get(stationID)
		if !ok {
			return Draft{}, fmt.Errorf("station %s not found", stationID)
		}
		celsius := desired[stationID]
		entry := DraftEntry{
			StationID: item.ID, StationName: item.Name, ZoneID: item.ZoneID,
			Online: item.Online, Maintenance: item.Maintenance, Hold: item.Hold,
			ControllerEpoch: item.ControllerEpoch, CurrentCelsius: item.TargetCelsius,
			DesiredCelsius: celsius, Changed: celsius != item.TargetCelsius,
		}
		entry.Issue = s.describeIssue(item, celsius)
		entries = append(entries, entry)
		zoneSet[item.ZoneID] = struct{}{}
	}
	zoneIDs := make([]string, 0, len(zoneSet))
	for zoneID := range zoneSet {
		zoneIDs = append(zoneIDs, zoneID)
	}
	sort.Strings(zoneIDs)
	now := time.Now().UTC()
	draft := Draft{
		ID: uuid.NewString(), SourceRevision: s.targets.Revision(), Entries: entries,
		ZoneIDs: zoneIDs, Note: note, CreatedBy: actor, CreatedAt: now, RefreshedAt: now,
	}
	s.mu.Lock()
	s.drafts[draft.ID] = draft.Clone()
	s.mu.Unlock()
	return draft.Clone(), nil
}

// RefreshDraft re-reads every controller snapshot and the dispatch
// revision, keeping the originally desired targets.
func (s *Service) RefreshDraft(draftID string) (Draft, error) {
	s.mu.Lock()
	draft, ok := s.drafts[draftID]
	s.mu.Unlock()
	if !ok {
		return Draft{}, fmt.Errorf("draft %s not found", draftID)
	}
	desired := make(map[string]float64, len(draft.Entries))
	for _, entry := range draft.Entries {
		desired[entry.StationID] = entry.DesiredCelsius
	}
	refreshed, err := s.CreateDraft(desired, draft.CreatedBy, draft.Note)
	if err != nil {
		return Draft{}, err
	}
	refreshed.ID = draft.ID
	refreshed.CreatedAt = draft.CreatedAt
	s.mu.Lock()
	s.drafts[draftID] = refreshed.Clone()
	s.mu.Unlock()
	return refreshed.Clone(), nil
}

func (s *Service) describeIssue(item station.Station, celsius float64) string {
	switch {
	case celsius < station.MinTargetCelsius || celsius > station.MaxTargetCelsius:
		return fmt.Sprintf("target %.1f outside operating envelope %.0f-%.0f", celsius, station.MinTargetCelsius, station.MaxTargetCelsius)
	case item.Maintenance:
		return "station in maintenance"
	case item.Hold:
		return "station manually held"
	case !item.Online:
		return "station offline"
	default:
		return ""
	}
}

func (s *Service) Draft(draftID string) (Draft, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	draft, ok := s.drafts[draftID]
	return draft.Clone(), ok
}

func (s *Service) Drafts() []Draft {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]Draft, 0, len(s.drafts))
	for _, draft := range s.drafts {
		result = append(result, draft.Clone())
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.After(result[j].CreatedAt) })
	return result
}

// CreateRelease turns a draft into an approval-gated batch. Every zone the
// draft touches starts pending.
func (s *Service) CreateRelease(draftID, actor, note string) (Release, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	draft, ok := s.drafts[draftID]
	if !ok {
		return Release{}, fmt.Errorf("draft %s not found", draftID)
	}
	stationIDsByZone := make(map[string][]string)
	for _, entry := range draft.Entries {
		stationIDsByZone[entry.ZoneID] = append(stationIDsByZone[entry.ZoneID], entry.StationID)
	}
	zones := make([]ZoneApproval, 0, len(draft.ZoneIDs))
	for _, zoneID := range draft.ZoneIDs {
		ids := stationIDsByZone[zoneID]
		sort.Strings(ids)
		name := zoneID
		if zone, ok := s.zones.Get(zoneID); ok {
			name = zone.Name
		}
		zones = append(zones, ZoneApproval{ZoneID: zoneID, ZoneName: name, StationIDs: ids, Decision: ZonePending})
	}
	release := Release{
		ID: uuid.NewString(), DraftID: draftID, SourceRevision: draft.SourceRevision,
		Actor: actor, Note: note, Status: ReleasePendingApproval, Zones: zones,
		CreatedAt: time.Now().UTC(),
	}
	s.releases[release.ID] = release.Clone()
	return release.Clone(), nil
}

// ApproveZone records a zone approval. Approving an already-rejected zone is
// rejected: a new release must be cut instead.
func (s *Service) ApproveZone(releaseID, zoneID, actor string) (Release, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.decideZone(releaseID, zoneID, ZoneApproved, actor, "")
}

// RejectZone blocks the whole release; a single rejected zone prevents the
// batch from ever publishing.
func (s *Service) RejectZone(releaseID, zoneID, actor, reason string) (Release, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.decideZone(releaseID, zoneID, ZoneRejected, actor, reason)
}

func (s *Service) decideZone(releaseID, zoneID string, decision ZoneDecision, actor, reason string) (Release, error) {
	release, ok := s.releases[releaseID]
	if !ok {
		return Release{}, fmt.Errorf("release %s not found", releaseID)
	}
	if release.Status.Terminal() {
		return release, fmt.Errorf("release %s is %s", releaseID, release.Status)
	}
	index := -1
	for i := range release.Zones {
		if release.Zones[i].ZoneID == zoneID {
			index = i
			break
		}
	}
	if index < 0 {
		return release, fmt.Errorf("zone %s is not part of release %s", zoneID, releaseID)
	}
	release.Zones[index].Decision = decision
	release.Zones[index].Actor = actor
	release.Zones[index].Reason = reason
	release.Zones[index].DecidedAt = time.Now().UTC()
	if decision == ZoneRejected {
		release.Status = ReleaseRejected
		release.Message = fmt.Sprintf("zone %s rejected by %s: %s", zoneID, actor, reason)
	}
	s.releases[releaseID] = release.Clone()
	return release.Clone(), nil
}

// Publish sends the batch after all zones approve. Targets and controller
// commands are applied per station, and every outcome is recorded. A
// release is only ReleaseSucceeded when every controller acknowledges; any
// failure yields ReleasePartialFailure or ReleaseFailed.
func (s *Service) Publish(releaseID, actor string) (Release, error) {
	s.mu.Lock()
	release, ok := s.releases[releaseID]
	if !ok {
		s.mu.Unlock()
		return Release{}, fmt.Errorf("release %s not found", releaseID)
	}
	if release.Status.Terminal() {
		s.mu.Unlock()
		return release, fmt.Errorf("release %s is %s", releaseID, release.Status)
	}
	if !release.AllZonesApproved() {
		pending := s.pendingZonesLocked(release)
		s.mu.Unlock()
		return release, fmt.Errorf("release %s awaiting zone approval: %v", releaseID, pending)
	}
	draft, ok := s.drafts[release.DraftID]
	if !ok {
		s.mu.Unlock()
		return Release{}, fmt.Errorf("draft %s for release %s not found", release.DraftID, releaseID)
	}
	targetsByStation := make(map[string]float64, len(draft.Entries))
	for _, entry := range draft.Entries {
		targetsByStation[entry.StationID] = entry.DesiredCelsius
	}
	s.mu.Unlock()

	targetRevision, targetOutcomes, stale := s.targets.PublishTargets(release.SourceRevision, targetsByStation)

	s.mu.Lock()
	defer s.mu.Unlock()
	release, ok = s.releases[releaseID]
	if !ok {
		return Release{}, fmt.Errorf("release %s vanished during publish", releaseID)
	}
	if stale {
		release.Status = ReleaseExpired
		release.TargetRevision = targetRevision
		release.Message = fmt.Sprintf("source revision %d stale; dispatch moved to %d", release.SourceRevision, targetRevision)
		release.Results = s.resultsForStale(release, draft)
		release.PublishedAt = time.Now().UTC()
		s.releases[releaseID] = release.Clone()
		_ = s.audit.Rejected("release.publish", release.ID, actor, release.Message, map[string]any{
			"source_revision": release.SourceRevision, "current_revision": targetRevision,
		})
		return release.Clone(), fmt.Errorf("%s", release.Message)
	}

	release.TargetRevision = targetRevision
	release.Results = s.dispatchLocked(release, draft, targetOutcomes)
	release.PublishedAt = time.Now().UTC()
	release.Status = aggregateStatus(release.Results)
	release.Message = summarize(release)
	s.releases[releaseID] = release.Clone()

	counts := release.Counts()
	switch release.Status {
	case ReleaseSucceeded:
		_ = s.audit.Accepted("release.publish", release.ID, actor, release.Message, map[string]any{
			"target_revision": targetRevision, "stations": counts.Stations, "applied": counts.Applied,
		})
	default:
		failures := make([]map[string]any, 0, counts.Failed)
		for _, result := range release.Results {
			if !result.Acknowledged {
				failures = append(failures, map[string]any{"station_id": result.StationID, "error": result.Error})
			}
		}
		_ = s.audit.Rejected("release.publish", release.ID, actor, release.Message, map[string]any{
			"target_revision": targetRevision, "applied": counts.Applied, "failed": counts.Failed, "failures": failures,
		})
	}
	return release.Clone(), nil
}

func (s *Service) pendingZonesLocked(release Release) []string {
	pending := make([]string, 0)
	for _, zone := range release.Zones {
		if zone.Decision != ZoneApproved {
			pending = append(pending, zone.ZoneID)
		}
	}
	return pending
}

func (s *Service) resultsForStale(release Release, draft Draft) []ControllerResult {
	results := make([]ControllerResult, 0, len(draft.Entries))
	for _, entry := range draft.Entries {
		results = append(results, ControllerResult{
			StationID: entry.StationID, StationName: entry.StationName, ZoneID: entry.ZoneID,
			Epoch: entry.ControllerEpoch, Celsius: entry.DesiredCelsius,
			Error: fmt.Sprintf("source revision %d stale; no command sent", release.SourceRevision),
		})
	}
	return results
}

// dispatchLocked sends one station command per applied target. Failed
// targets produce a result carrying the reason and no command.
func (s *Service) dispatchLocked(release Release, draft Draft, targetOutcomes []station.TargetOutcome) []ControllerResult {
	outcomeByStation := make(map[string]station.TargetOutcome, len(targetOutcomes))
	for _, outcome := range targetOutcomes {
		outcomeByStation[outcome.StationID] = outcome
	}
	results := make([]ControllerResult, 0, len(draft.Entries))
	for _, entry := range draft.Entries {
		result := ControllerResult{
			StationID: entry.StationID, StationName: entry.StationName, ZoneID: entry.ZoneID,
			Celsius: entry.DesiredCelsius, Epoch: entry.ControllerEpoch,
		}
		outcome := outcomeByStation[entry.StationID]
		if !outcome.Applied {
			result.Error = outcome.Error
			if result.Error == "" {
				result.Error = "target not applied"
			}
			results = append(results, result)
			continue
		}
		result.TargetApplied = true
		// Re-read the controller so the command carries the epoch it is
		// actually fencing on right now.
		item, ok := s.stations.Get(entry.StationID)
		if !ok {
			result.Error = "station vanished before command dispatch"
			results = append(results, result)
			continue
		}
		if !item.Online {
			result.Error = "controller offline; no receipt"
			results = append(results, result)
			continue
		}
		result.Epoch = item.ControllerEpoch
		// Seed from any command previously observed (e.g. reconnect
		// replays) so our sequence never collides with one already seen
		// by the command service.
		if s.sequences[entry.StationID] == 0 {
			if last, hasLast := s.commands.Last(entry.StationID); hasLast && last.Sequence > s.sequences[entry.StationID] {
				s.sequences[entry.StationID] = last.Sequence
			}
		}
		s.sequences[entry.StationID]++
		result.Sequence = s.sequences[entry.StationID]
		result.CommandID = uuid.NewString()
		now := time.Now().UTC()
		result.At = now
		receipt, err := s.commands.Apply(station.Command{
			ID: result.CommandID, StationID: entry.StationID, Epoch: result.Epoch,
			Sequence: result.Sequence, Celsius: result.Celsius, At: now,
		})
		result.ReceiptStatus = receipt.Status
		switch {
		case err != nil:
			result.Error = err.Error()
		case receipt.Status == "applied" || receipt.Status == "duplicate":
			result.Acknowledged = true
			if receipt.ID != "" {
				result.CommandID = receipt.ID
			}
		default:
			result.Error = "receipt status: " + receipt.Status
		}
		results = append(results, result)
	}
	return results
}

func aggregateStatus(results []ControllerResult) ReleaseStatus {
	applied, failed := 0, 0
	for _, result := range results {
		if result.Acknowledged {
			applied++
		} else {
			failed++
		}
	}
	switch {
	case failed == 0 && applied > 0:
		return ReleaseSucceeded
	case applied > 0:
		return ReleasePartialFailure
	default:
		return ReleaseFailed
	}
}

func summarize(release Release) string {
	counts := release.Counts()
	switch release.Status {
	case ReleaseSucceeded:
		return fmt.Sprintf("release published: %d/%d controllers acknowledged", counts.Applied, counts.Stations)
	case ReleasePartialFailure:
		return fmt.Sprintf("partial failure: %d/%d acknowledged, %d failed", counts.Applied, counts.Stations, counts.Failed)
	default:
		return fmt.Sprintf("release failed: 0/%d controllers acknowledged", counts.Stations)
	}
}

func (s *Service) Release(releaseID string) (Release, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	release, ok := s.releases[releaseID]
	return release.Clone(), ok
}

func (s *Service) Releases() []Release {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]Release, 0, len(s.releases))
	for _, release := range s.releases {
		result = append(result, release.Clone())
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.After(result[j].CreatedAt) })
	return result
}
