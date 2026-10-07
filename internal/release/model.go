package release

import "time"

// ReleaseStatus is the aggregate state of a whole release. Any terminal
// state other than ReleaseSucceeded means at least one station did not
// receive its command; callers must never render ReleasePartialFailure as
// success.
type ReleaseStatus string

const (
	ReleasePendingApproval ReleaseStatus = "pending_approval"
	ReleaseRejected        ReleaseStatus = "rejected"
	ReleaseExpired         ReleaseStatus = "expired"
	ReleaseSucceeded       ReleaseStatus = "succeeded"
	ReleasePartialFailure  ReleaseStatus = "partial_failure"
	ReleaseFailed          ReleaseStatus = "failed"
)

// Terminal reports whether no further zone decision or publish is possible.
func (s ReleaseStatus) Terminal() bool {
	switch s {
	case ReleaseRejected, ReleaseExpired, ReleaseSucceeded, ReleasePartialFailure, ReleaseFailed:
		return true
	default:
		return false
	}
}

type ZoneDecision string

const (
	ZonePending  ZoneDecision = "pending"
	ZoneApproved ZoneDecision = "approved"
	ZoneRejected ZoneDecision = "rejected"
)

// DraftEntry compares the currently running target of one controller with
// the desired target captured for an upcoming release.
type DraftEntry struct {
	StationID       string  `json:"station_id"`
	StationName     string  `json:"station_name"`
	ZoneID          string  `json:"zone_id"`
	Online          bool    `json:"online"`
	Maintenance     bool    `json:"maintenance"`
	Hold            bool    `json:"hold"`
	ControllerEpoch uint64  `json:"controller_epoch"`
	CurrentCelsius  float64 `json:"current_celsius"`
	DesiredCelsius  float64 `json:"desired_celsius"`
	Changed         bool    `json:"changed"`
	// Issue carries a draft-time advisory (out-of-envelope target, held or
	// maintenance station). A draft with issues can still be saved, but the
	// affected stations will fail at publish unless the issue clears.
	Issue string `json:"issue,omitempty"`
}

// Draft is the target proposal generated before any approval happens. It
// pins the target revision so a publish can detect concurrent dispatch.
type Draft struct {
	ID             string       `json:"id"`
	SourceRevision uint64       `json:"source_revision"`
	Entries        []DraftEntry `json:"entries"`
	ZoneIDs        []string     `json:"zone_ids"`
	Note           string       `json:"note,omitempty"`
	CreatedBy      string       `json:"created_by"`
	CreatedAt      time.Time    `json:"created_at"`
	RefreshedAt    time.Time    `json:"refreshed_at"`
}

// ZoneApproval is the per-zone gate. Every zone touched by a release must be
// approved before the batch may be published.
type ZoneApproval struct {
	ZoneID     string       `json:"zone_id"`
	ZoneName   string       `json:"zone_name"`
	StationIDs []string     `json:"station_ids"`
	Decision   ZoneDecision `json:"decision"`
	Actor      string       `json:"actor,omitempty"`
	Reason     string       `json:"reason,omitempty"`
	DecidedAt  time.Time    `json:"decided_at,omitempty"`
}

// ControllerResult tracks a single controller through batch publish: the
// epoch/sequence it was commanded on and the receipt returned by the
// station command service.
type ControllerResult struct {
	StationID     string  `json:"station_id"`
	StationName   string  `json:"station_name"`
	ZoneID        string  `json:"zone_id"`
	Epoch         uint64  `json:"epoch"`
	Sequence      uint64  `json:"sequence"`
	CommandID     string  `json:"command_id,omitempty"`
	Celsius       float64 `json:"celsius"`
	TargetApplied bool    `json:"target_applied"`
	// ReceiptStatus mirrors station.Command.Status (applied, duplicate,
	// rejected_old_epoch, ...) and is empty when no command was sent.
	ReceiptStatus string    `json:"receipt_status,omitempty"`
	Acknowledged  bool      `json:"acknowledged"`
	Error         string    `json:"error,omitempty"`
	At            time.Time `json:"at,omitempty"`
}

// Release is an approved-or-pending batch of station commands derived from a
// draft.
type Release struct {
	ID             string `json:"id"`
	DraftID        string `json:"draft_id"`
	SourceRevision uint64 `json:"source_revision"`
	// TargetRevision is the dispatch revision produced by publish; zero
	// before publish.
	TargetRevision uint64             `json:"target_revision,omitempty"`
	Actor          string             `json:"actor"`
	Note           string             `json:"note,omitempty"`
	Status         ReleaseStatus      `json:"status"`
	Message        string             `json:"message,omitempty"`
	Zones          []ZoneApproval     `json:"zones"`
	Results        []ControllerResult `json:"results,omitempty"`
	CreatedAt      time.Time          `json:"created_at"`
	PublishedAt    time.Time          `json:"published_at,omitempty"`
}

// AllZonesApproved reports whether the release still has a publishable gate.
func (r Release) AllZonesApproved() bool {
	if len(r.Zones) == 0 {
		return false
	}
	for _, zone := range r.Zones {
		if zone.Decision != ZoneApproved {
			return false
		}
	}
	return true
}

// Counts summarizes per-controller outcomes for UI rendering.
type Counts struct {
	Stations int `json:"stations"`
	Applied  int `json:"applied"`
	Failed   int `json:"failed"`
}

func (r Release) Counts() Counts {
	counts := Counts{Stations: len(r.Results)}
	for _, result := range r.Results {
		if result.Acknowledged {
			counts.Applied++
		} else {
			counts.Failed++
		}
	}
	return counts
}

func (d Draft) Clone() Draft {
	copy := d
	copy.Entries = append([]DraftEntry(nil), d.Entries...)
	copy.ZoneIDs = append([]string(nil), d.ZoneIDs...)
	return copy
}

func (r Release) Clone() Release {
	copy := r
	copy.Zones = make([]ZoneApproval, len(r.Zones))
	for i, zone := range r.Zones {
		zone.StationIDs = append([]string(nil), zone.StationIDs...)
		copy.Zones[i] = zone
	}
	copy.Results = append([]ControllerResult(nil), r.Results...)
	return copy
}
