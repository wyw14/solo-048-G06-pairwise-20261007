package station

import (
	"fmt"
	"sync"
	"time"
)

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

// MinTargetCelsius and MaxTargetCelsius bound the operating envelope that
// every published station target must stay within.
const (
	MinTargetCelsius = 35.0
	MaxTargetCelsius = 95.0
)

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

// ApplyTarget validates and writes a single station target, advancing the
// target revision only for that station. Multi-station release batches call it
// once per station so one rejection does not roll back or abort the others.
func (s *TargetService) ApplyTarget(stationID string, celsius float64) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if celsius < MinTargetCelsius || celsius > MaxTargetCelsius {
		return 0, fmt.Errorf("target %.1f for %s outside operating envelope", celsius, stationID)
	}
	_, err := s.registry.Update(stationID, func(item *Station) error {
		if item.Maintenance {
			return fmt.Errorf("station %s is in maintenance", stationID)
		}
		if item.Hold {
			return fmt.Errorf("station %s is manually held", stationID)
		}
		item.TargetCelsius = celsius
		item.TargetRevision = s.revision + 1
		return nil
	})
	if err != nil {
		return 0, err
	}
	s.revision++
	return s.revision, nil
}
