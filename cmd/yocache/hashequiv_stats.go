package main

import (
	"math"
	"sync"
)

type hashEquivRequestStats struct {
	mu        sync.Mutex
	num       uint64
	totalTime float64
	maxTime   float64
	mean      float64
	squared   float64
}

type hashEquivStatsSnapshot struct {
	Num       uint64  `json:"num"`
	TotalTime float64 `json:"total_time"`
	MaxTime   float64 `json:"max_time"`
	Average   float64 `json:"average"`
	Stdev     float64 `json:"stdev"`
}

func (s *hashEquivRequestStats) add(elapsed float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.num++
	delta := elapsed - s.mean
	s.mean += delta / float64(s.num)
	s.squared += delta * (elapsed - s.mean)
	s.totalTime += elapsed
	s.maxTime = math.Max(s.maxTime, elapsed)
}

func (s *hashEquivRequestStats) snapshot() hashEquivStatsSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := hashEquivStatsSnapshot{Num: s.num, TotalTime: s.totalTime, MaxTime: s.maxTime}
	if s.num > 0 {
		result.Average = s.totalTime / float64(s.num)
	}
	if s.num > 1 {
		result.Stdev = math.Sqrt(s.squared / float64(s.num-1))
	}
	return result
}
