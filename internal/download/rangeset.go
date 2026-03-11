package download

import (
	"encoding/json"
	"sort"
)

// Interval represents a half-open byte range [Start, End).
type Interval struct {
	Start int64
	End   int64
}

// RangeSet is a sorted, auto-merging set of non-overlapping [Start, End) intervals.
// It tracks which byte ranges of a file are available on disk.
type RangeSet struct {
	intervals []Interval
}

// NewRangeSet returns an empty RangeSet.
func NewRangeSet() *RangeSet {
	return &RangeSet{}
}

// Add inserts the interval [offset, offset+length) and merges any overlapping
// or adjacent intervals.
func (rs *RangeSet) Add(offset, length int64) {
	if length <= 0 {
		return
	}
	newIv := Interval{Start: offset, End: offset + length}

	// Find the position where this interval starts affecting existing ones.
	// We merge with any interval that overlaps or is adjacent.
	merged := make([]Interval, 0, len(rs.intervals)+1)
	inserted := false

	for _, iv := range rs.intervals {
		if iv.End < newIv.Start {
			// iv is entirely before newIv, keep it.
			merged = append(merged, iv)
		} else if iv.Start > newIv.End {
			// iv is entirely after newIv. Insert newIv first if not yet done.
			if !inserted {
				merged = append(merged, newIv)
				inserted = true
			}
			merged = append(merged, iv)
		} else {
			// Overlapping or adjacent — expand newIv to cover both.
			if iv.Start < newIv.Start {
				newIv.Start = iv.Start
			}
			if iv.End > newIv.End {
				newIv.End = iv.End
			}
		}
	}
	if !inserted {
		merged = append(merged, newIv)
	}

	rs.intervals = merged
}

// Contains returns true if the entire range [offset, offset+length) is covered.
func (rs *RangeSet) Contains(offset, length int64) bool {
	if length <= 0 {
		return true
	}
	target := Interval{Start: offset, End: offset + length}

	idx := sort.Search(len(rs.intervals), func(i int) bool {
		return rs.intervals[i].End > target.Start
	})
	if idx >= len(rs.intervals) {
		return false
	}
	iv := rs.intervals[idx]
	return iv.Start <= target.Start && iv.End >= target.End
}

// IsComplete returns true if [0, totalSize) is fully covered.
func (rs *RangeSet) IsComplete(totalSize int64) bool {
	if totalSize <= 0 {
		return true
	}
	return rs.Contains(0, totalSize)
}

// Gaps returns the uncovered intervals within [0, totalSize).
func (rs *RangeSet) Gaps(totalSize int64) []Interval {
	if totalSize <= 0 {
		return nil
	}
	var gaps []Interval
	pos := int64(0)
	for _, iv := range rs.intervals {
		if iv.Start >= totalSize {
			break
		}
		if iv.Start > pos {
			end := iv.Start
			if end > totalSize {
				end = totalSize
			}
			gaps = append(gaps, Interval{Start: pos, End: end})
		}
		if iv.End > pos {
			pos = iv.End
		}
	}
	if pos < totalSize {
		gaps = append(gaps, Interval{Start: pos, End: totalSize})
	}
	return gaps
}

// Len returns the number of intervals.
func (rs *RangeSet) Len() int {
	return len(rs.intervals)
}

// Intervals returns a copy of the intervals slice.
func (rs *RangeSet) Intervals() []Interval {
	out := make([]Interval, len(rs.intervals))
	copy(out, rs.intervals)
	return out
}

// MarshalJSON serializes the RangeSet to [[start,end], ...] format.
func (rs *RangeSet) MarshalJSON() ([]byte, error) {
	pairs := make([][2]int64, len(rs.intervals))
	for i, iv := range rs.intervals {
		pairs[i] = [2]int64{iv.Start, iv.End}
	}
	return json.Marshal(pairs)
}

// UnmarshalJSON deserializes from [[start,end], ...] format.
func (rs *RangeSet) UnmarshalJSON(data []byte) error {
	var pairs [][2]int64
	if err := json.Unmarshal(data, &pairs); err != nil {
		return err
	}
	rs.intervals = make([]Interval, len(pairs))
	for i, p := range pairs {
		rs.intervals[i] = Interval{Start: p[0], End: p[1]}
	}
	return nil
}

// String returns a human-readable representation.
func (rs *RangeSet) String() string {
	data, _ := rs.MarshalJSON()
	return string(data)
}
