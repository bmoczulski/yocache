package main

import (
	"math"
	"sync"
)

type hashEquivRequestStats struct {
	mu        sync.Mutex
	num       uint64
	totalTime float64
	totalComp [2]float64 // what adding to totalTime dropped; see addTotalLocked
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

// add records one request's elapsed time. mean and squared follow Welford's
// online algorithm, as bitbake's hashserv does, so stdev stays accurate
// without keeping the samples:
// https://en.wikipedia.org/wiki/Algorithms_for_calculating_variance#Welford%27s_online_algorithm
func (s *hashEquivRequestStats) add(elapsed float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.num++
	delta := elapsed - s.mean
	s.mean += delta / float64(s.num)
	s.squared += delta * (elapsed - s.mean)
	s.addTotalLocked(elapsed)
	s.maxTime = math.Max(s.maxTime, elapsed)
}

// addTotalLocked adds elapsed to totalTime with Klein's second-order
// compensated summation. Over millions of microsecond-sized samples a plain
// float sum drifts as each sample's low bits round away against the growing
// total. totalComp[0] collects what each addition dropped, the way Neumaier's
// method does, and totalComp[1] collects what totalComp[0] itself drops while
// doing so; snapshot adds them back once. Caller holds s.mu.
// https://en.wikipedia.org/wiki/Kahan_summation_algorithm#Further_enhancements
func (s *hashEquivRequestStats) addTotalLocked(elapsed float64) {
	var c, cc float64
	s.totalTime, c = twoSum(s.totalTime, elapsed)
	s.totalComp[0], cc = twoSum(s.totalComp[0], c)
	s.totalComp[1] += cc
}

// totalLocked returns totalTime with its compensation applied. The order
// matters: totalComp[0] can cancel most of totalTime, and only then is
// totalComp[1] large enough relative to the remainder not to round away.
// Caller holds s.mu.
func (s *hashEquivRequestStats) totalLocked() float64 {
	return (s.totalTime + s.totalComp[0]) + s.totalComp[1]
}

// twoSum returns a+b rounded to float64 and the exact rounding error, so that
// a + b == sum + err holds exactly (Knuth's 2Sum). Unlike Dekker's cheaper
// Fast2Sum it needs no |a| >= |b| precondition, so no branch.
// https://en.wikipedia.org/wiki/2Sum
func twoSum(a, b float64) (sum, err float64) {
	sum = a + b
	back := sum - a
	err = (a - (sum - back)) + (b - back)
	return sum, err
}

func (s *hashEquivRequestStats) snapshot() hashEquivStatsSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := s.totalLocked()
	result := hashEquivStatsSnapshot{Num: s.num, TotalTime: total, MaxTime: s.maxTime}
	if s.num > 0 {
		result.Average = total / float64(s.num)
	}
	if s.num > 1 {
		result.Stdev = math.Sqrt(s.squared / float64(s.num-1))
	}
	return result
}
