package station

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

const (
	MinTargetCelsius = 35.0
	MaxTargetCelsius = 95.0
)

var (
	ErrStaleRevision = errors.New("dispatch source revision is stale")
	ErrStationHeld   = errors.New("station is manually held")
)

// TargetOutcome records the per-station result of a batch target publish.
type TargetOutcome struct {
	StationID string  `json:"station_id"`
	Celsius   float64 `json:"celsius"`
	Applied   bool    `json:"applied"`
	Error     string  `json:"error,omitempty"`
}

type TargetService struct {
	registry *Registry
	mu       sync.Mutex
	revision uint64
}

func NewTargetService(registry *Registry) *TargetService {
	var revision uint64
	for _, item := range registry.List() {
		if item.TargetRevision > revision {
			revision = item.TargetRevision
		}
	}
	return &TargetService{registry: registry, revision: revision}
}

func (s *TargetService) Revision() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revision
}

func (s *TargetService) ManualHold(stationIDs []string, actor string) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revision++
	for _, stationID := range stationIDs {
		if _, err := s.registry.Update(stationID, func(item *Station) error {
			item.Hold = true
			item.TargetRevision = s.revision
			return nil
		}); err != nil {
			return 0, fmt.Errorf("hold %s by %s: %w", stationID, actor, err)
		}
	}
	return s.revision, nil
}

func (s *TargetService) ReleaseHold(stationID string) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revision++
	_, err := s.registry.Update(stationID, func(item *Station) error {
		item.Hold = false
		item.TargetRevision = s.revision
		return nil
	})
	return s.revision, err
}

func (s *TargetService) CompareAndPublish(sourceRevision uint64, targets map[string]float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sourceRevision != s.revision {
		return fmt.Errorf("dispatch source revision %d is stale; current is %d", sourceRevision, s.revision)
	}
	s.revision++
	for stationID, celsius := range targets {
		if celsius < MinTargetCelsius || celsius > MaxTargetCelsius {
			return fmt.Errorf("target %.1f for %s outside operating envelope", celsius, stationID)
		}
		_, err := s.registry.Update(stationID, func(item *Station) error {
			if item.Hold {
				return fmt.Errorf("station %s is manually held", stationID)
			}
			item.TargetCelsius = celsius
			item.TargetRevision = s.revision
			item.UpdatedAt = time.Now().UTC()
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// PublishTargets validates the revision fence once, then applies every target
// independently so a single rejected station never aborts the rest of the
// batch. The revision is bumped exactly once when the fence is current, even
// when some stations are rejected; callers must inspect every returned
// outcome instead of treating a non-nil revision as whole-batch success.
func (s *TargetService) PublishTargets(sourceRevision uint64, targets map[string]float64) (uint64, []TargetOutcome, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	outcomes := make([]TargetOutcome, 0, len(targets))
	if sourceRevision != s.revision {
		for stationID, celsius := range targets {
			outcomes = append(outcomes, TargetOutcome{
				StationID: stationID, Celsius: celsius,
				Error: fmt.Sprintf("dispatch source revision %d is stale; current is %d", sourceRevision, s.revision),
			})
		}
		sort.Slice(outcomes, func(i, j int) bool { return outcomes[i].StationID < outcomes[j].StationID })
		return s.revision, outcomes, true
	}
	s.revision++
	revision := s.revision
	for stationID, celsius := range targets {
		outcome := TargetOutcome{StationID: stationID, Celsius: celsius}
		switch {
		case celsius < MinTargetCelsius || celsius > MaxTargetCelsius:
			outcome.Error = fmt.Sprintf("target %.1f for %s outside operating envelope", celsius, stationID)
		default:
			_, err := s.registry.Update(stationID, func(item *Station) error {
				if item.Hold {
					return fmt.Errorf("%w: %s", ErrStationHeld, stationID)
				}
				item.TargetCelsius = celsius
				item.TargetRevision = revision
				item.UpdatedAt = time.Now().UTC()
				return nil
			})
			if err != nil {
				outcome.Error = err.Error()
			} else {
				outcome.Applied = true
			}
		}
		outcomes = append(outcomes, outcome)
	}
	sort.Slice(outcomes, func(i, j int) bool { return outcomes[i].StationID < outcomes[j].StationID })
	return revision, outcomes, false
}
