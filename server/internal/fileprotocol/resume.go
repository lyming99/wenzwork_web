package fileprotocol

import (
	"errors"
	"sort"
	"sync"
)

var ErrGenerationStale = errors.New("file transfer generation is stale")

type ChunkRange struct {
	Start uint64
	End   uint64
}

// ResumeState is an in-memory POC of the range state that desktop Agents must
// persist in SQLite only after staging bytes are durably checkpointed.
type ResumeState struct {
	mu         sync.Mutex
	generation uint64
	durable    map[uint64]struct{}
}

func NewResumeState(generation uint64) *ResumeState {
	return &ResumeState{generation: generation, durable: make(map[uint64]struct{})}
}

func (s *ResumeState) FenceGeneration(generation uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation <= s.generation {
		return ErrGenerationStale
	}
	s.generation = generation
	return nil
}

func (s *ResumeState) MarkDurable(generation, chunkIndex uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation != s.generation {
		return ErrGenerationStale
	}
	s.durable[chunkIndex] = struct{}{}
	return nil
}

func (s *ResumeState) Missing(generation, totalChunks uint64, maxRanges int) ([]ChunkRange, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation != s.generation {
		return nil, false, ErrGenerationStale
	}
	if maxRanges <= 0 {
		return nil, false, errors.New("max ranges must be positive")
	}
	missing := make([]uint64, 0)
	for index := uint64(0); index < totalChunks; index++ {
		if _, ok := s.durable[index]; !ok {
			missing = append(missing, index)
		}
	}
	if len(missing) == 0 {
		return nil, false, nil
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i] < missing[j] })
	ranges := make([]ChunkRange, 0, min(maxRanges, len(missing)))
	current := ChunkRange{Start: missing[0], End: missing[0] + 1}
	more := false
	for _, index := range missing[1:] {
		if index == current.End {
			current.End++
			continue
		}
		ranges = append(ranges, current)
		if len(ranges) == maxRanges {
			more = true
			return ranges, more, nil
		}
		current = ChunkRange{Start: index, End: index + 1}
	}
	ranges = append(ranges, current)
	if len(ranges) > maxRanges {
		ranges = ranges[:maxRanges]
		more = true
	}
	return ranges, more, nil
}
