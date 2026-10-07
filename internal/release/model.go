package release

import (
	"math"
	"sort"
	"time"
)

// Status is the lifecycle state of a target draft / release batch.
type Status string

const (
	StatusDraft       Status = "draft"        // draft being prepared and compared
	StatusStale       Status = "stale"        // live targets changed underneath the draft
	StatusApproved    Status = "approved"     // every affected zone has approved
	StatusPublishing  Status = "publishing"   // commands sent, waiting for receipts
	StatusSucceeded   Status = "succeeded"    // every targeted controller acknowledged
	StatusPartialFail Status = "partial_fail" // some controllers failed; the batch is NOT a success
	StatusFailed      Status = "failed"       // every controller failed (or rejected before sending)
)

// Terminal reports whether the release will no longer change.
func (s Status) Terminal() bool {
	return s == StatusSucceeded || s == StatusPartialFail || s == StatusFailed || s == StatusStale
}

// ZoneApproval tracks one zone's sign-off for a release.
type ZoneApproval struct {
	ZoneID     string    `json:"zone_id"`
	Approved   bool      `json:"approved"`
	ApprovedBy string    `json:"approved_by,omitempty"`
	ApprovedAt time.Time `json:"approved_at,omitempty"`
}

// StationChange is the per-controller projection inside a draft.
type StationChange struct {
	StationID     string  `json:"station_id"`
	Name          string  `json:"name"`
	ZoneID        string  `json:"zone_id"`
	Current       float64 `json:"current_celsius"`
	Proposed      float64 `json:"proposed_celsius"`
	Delta         float64 `json:"delta_celsius"`
	Changed       bool    `json:"changed"`
	Online        bool    `json:"online"`
	OnHold        bool    `json:"hold"`
	InMaintenance bool    `json:"maintenance"`
	CurrentEpoch  uint64  `json:"current_epoch"`
	Note          string  `json:"note,omitempty"`
}

// ResultStatus is the per-controller outcome of publishing one command.
type ResultStatus string

const (
	ResultPending      ResultStatus = "pending"      // command created, receipt not yet received
	ResultAcknowledged ResultStatus = "acknowledged" // controller receipt confirmed
	ResultRejected     ResultStatus = "rejected"     // command service refused the command
	ResultOffline      ResultStatus = "offline"      // controller not reachable at publish time
	ResultStaleEpoch   ResultStatus = "stale_epoch"  // controller had moved to a newer epoch
	ResultNoReceipt    ResultStatus = "no_receipt"   // never acknowledged within the window
)

// StationResult tracks the epoch, command and receipt for one controller.
type StationResult struct {
	StationID      string       `json:"station_id"`
	ZoneID         string       `json:"zone_id"`
	RequestedEpoch uint64       `json:"requested_epoch"` // epoch the command carried
	AppliedEpoch   uint64       `json:"applied_epoch"`   // epoch the controller reported on receipt
	Sequence       uint64       `json:"sequence"`
	CommandID      string       `json:"command_id,omitempty"`
	Status         ResultStatus `json:"status"`
	Detail         string       `json:"detail,omitempty"`
	TargetCelsius  float64      `json:"target_celsius"`
	SentAt         time.Time    `json:"sent_at,omitempty"`
	AckedAt        time.Time    `json:"acked_at,omitempty"`
}

// OK reports whether this controller accepted the new target.
func (r StationResult) OK() bool {
	return r.Status == ResultAcknowledged
}

// Draft is a generated target proposal with a comparison against live settings.
type Draft struct {
	ID             string                  `json:"id"`
	SourceRevision uint64                  `json:"source_revision"`
	Status         Status                  `json:"status"`
	Changes        []StationChange         `json:"changes"`
	Approvals      map[string]ZoneApproval `json:"approvals"`
	CreatedAt      time.Time               `json:"created_at"`
	UpdatedAt      time.Time               `json:"updated_at"`
	ReleaseID      string                  `json:"release_id,omitempty"`
	Message        string                  `json:"message,omitempty"`
}

// Clone returns a deep copy safe to hand out under no lock.
func (d Draft) Clone() Draft {
	copy := d
	copy.Changes = append([]StationChange(nil), d.Changes...)
	if d.Approvals != nil {
		copy.Approvals = make(map[string]ZoneApproval, len(d.Approvals))
		for zoneID, approval := range d.Approvals {
			copy.Approvals[zoneID] = approval
		}
	}
	return copy
}

// Zones returns the sorted zone IDs touched by the draft.
func (d Draft) Zones() []string {
	seen := make(map[string]struct{})
	for _, change := range d.Changes {
		seen[change.ZoneID] = struct{}{}
	}
	zones := make([]string, 0, len(seen))
	for zoneID := range seen {
		zones = append(zones, zoneID)
	}
	sort.Strings(zones)
	return zones
}

// Approved reports whether every affected zone has signed off.
func (d Draft) Approved() bool {
	if len(d.Changes) == 0 {
		return false
	}
	for _, zoneID := range d.Zones() {
		if !d.Approvals[zoneID].Approved {
			return false
		}
	}
	return true
}

// HasChanges reports whether the draft proposes anything different from live.
func (d Draft) HasChanges() bool {
	for _, change := range d.Changes {
		if change.Changed {
			return true
		}
	}
	return false
}

// Release is a batch published from an approved draft.
type Release struct {
	ID          string                   `json:"id"`
	DraftID     string                   `json:"draft_id"`
	Status      Status                   `json:"status"`
	Results     map[string]StationResult `json:"results"`
	CreatedAt   time.Time                `json:"created_at"`
	UpdatedAt   time.Time                `json:"updated_at"`
	Message     string                   `json:"message,omitempty"`
	PublishedBy string                   `json:"published_by,omitempty"`
}

// Clone returns a deep copy.
func (r Release) Clone() Release {
	copy := r
	if r.Results != nil {
		copy.Results = make(map[string]StationResult, len(r.Results))
		for stationID, result := range r.Results {
			copy.Results[stationID] = result
		}
	}
	return copy
}

// Counts returns success, pending and failure tallies across controllers.
func (r Release) Counts() (success, pending, failed int) {
	for _, result := range r.Results {
		switch {
		case result.OK():
			success++
		case result.Status == ResultPending:
			pending++
		default:
			failed++
		}
	}
	return success, pending, failed
}

func temperatureEqual(a, b float64) bool {
	return math.Abs(a-b) < 0.0001
}
