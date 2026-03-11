package download

import (
	"encoding/json"
	"testing"
)

func TestAddSingle(t *testing.T) {
	rs := NewRangeSet()
	rs.Add(0, 100)
	if got := rs.Len(); got != 1 {
		t.Fatalf("expected 1 interval, got %d", got)
	}
	iv := rs.Intervals()[0]
	if iv.Start != 0 || iv.End != 100 {
		t.Fatalf("unexpected interval: %v", iv)
	}
}

func TestAddNonOverlapping(t *testing.T) {
	rs := NewRangeSet()
	rs.Add(0, 100)
	rs.Add(200, 100) // [0,100) [200,300)
	if got := rs.Len(); got != 2 {
		t.Fatalf("expected 2 intervals, got %d", got)
	}
}

func TestAddAdjacent(t *testing.T) {
	rs := NewRangeSet()
	rs.Add(0, 100)
	rs.Add(100, 100) // should merge into [0,200)
	if got := rs.Len(); got != 1 {
		t.Fatalf("expected 1 interval after merge, got %d", got)
	}
	iv := rs.Intervals()[0]
	if iv.Start != 0 || iv.End != 200 {
		t.Fatalf("unexpected merged interval: %v", iv)
	}
}

func TestAddOverlapping(t *testing.T) {
	rs := NewRangeSet()
	rs.Add(0, 100)
	rs.Add(50, 100) // overlap → [0,150)
	if got := rs.Len(); got != 1 {
		t.Fatalf("expected 1 interval, got %d", got)
	}
	iv := rs.Intervals()[0]
	if iv.Start != 0 || iv.End != 150 {
		t.Fatalf("unexpected merged interval: %v", iv)
	}
}

func TestAddMergeMultiple(t *testing.T) {
	rs := NewRangeSet()
	rs.Add(0, 100)
	rs.Add(200, 100)
	rs.Add(400, 100)
	// Now bridge [0-100) and [200-300) and [400-500)
	rs.Add(50, 450) // should merge all into [0,500)
	if got := rs.Len(); got != 1 {
		t.Fatalf("expected 1 interval, got %d", got)
	}
	iv := rs.Intervals()[0]
	if iv.Start != 0 || iv.End != 500 {
		t.Fatalf("unexpected merged interval: %v", iv)
	}
}

func TestAddZeroLength(t *testing.T) {
	rs := NewRangeSet()
	rs.Add(0, 0)
	if got := rs.Len(); got != 0 {
		t.Fatalf("expected 0 intervals, got %d", got)
	}
}

func TestAddNegativeLength(t *testing.T) {
	rs := NewRangeSet()
	rs.Add(0, -5)
	if got := rs.Len(); got != 0 {
		t.Fatalf("expected 0 intervals, got %d", got)
	}
}

func TestAddInsertBefore(t *testing.T) {
	rs := NewRangeSet()
	rs.Add(200, 100)
	rs.Add(0, 50) // insert before existing
	if got := rs.Len(); got != 2 {
		t.Fatalf("expected 2 intervals, got %d", got)
	}
	ivs := rs.Intervals()
	if ivs[0].Start != 0 || ivs[0].End != 50 {
		t.Fatalf("first interval wrong: %v", ivs[0])
	}
	if ivs[1].Start != 200 || ivs[1].End != 300 {
		t.Fatalf("second interval wrong: %v", ivs[1])
	}
}

func TestContains(t *testing.T) {
	rs := NewRangeSet()
	rs.Add(0, 100)
	rs.Add(200, 100)

	tests := []struct {
		off, len int64
		want     bool
	}{
		{0, 100, true},
		{0, 50, true},
		{50, 50, true},
		{0, 101, false},  // extends past first interval
		{99, 2, false},   // straddles gap
		{200, 100, true}, // second interval
		{200, 50, true},
		{150, 10, false}, // in the gap
		{0, 0, true},     // zero length always true
	}

	for _, tc := range tests {
		got := rs.Contains(tc.off, tc.len)
		if got != tc.want {
			t.Errorf("Contains(%d, %d) = %v, want %v", tc.off, tc.len, got, tc.want)
		}
	}
}

func TestIsComplete(t *testing.T) {
	rs := NewRangeSet()
	rs.Add(0, 100)

	if !rs.IsComplete(100) {
		t.Error("expected complete for totalSize=100")
	}
	if !rs.IsComplete(50) {
		t.Error("expected complete for totalSize=50")
	}
	if rs.IsComplete(101) {
		t.Error("expected incomplete for totalSize=101")
	}
	if !rs.IsComplete(0) {
		t.Error("expected complete for totalSize=0")
	}
}

func TestGaps(t *testing.T) {
	rs := NewRangeSet()
	rs.Add(10, 20) // [10,30)
	rs.Add(50, 10) // [50,60)

	gaps := rs.Gaps(100)
	expected := []Interval{
		{0, 10},
		{30, 50},
		{60, 100},
	}
	if len(gaps) != len(expected) {
		t.Fatalf("expected %d gaps, got %d: %v", len(expected), len(gaps), gaps)
	}
	for i, g := range gaps {
		if g != expected[i] {
			t.Errorf("gap[%d] = %v, want %v", i, g, expected[i])
		}
	}
}

func TestGapsComplete(t *testing.T) {
	rs := NewRangeSet()
	rs.Add(0, 100)
	gaps := rs.Gaps(100)
	if len(gaps) != 0 {
		t.Fatalf("expected no gaps, got %v", gaps)
	}
}

func TestGapsEmpty(t *testing.T) {
	rs := NewRangeSet()
	gaps := rs.Gaps(100)
	if len(gaps) != 1 || gaps[0].Start != 0 || gaps[0].End != 100 {
		t.Fatalf("expected single gap [0,100), got %v", gaps)
	}
}

func TestJSONRoundTrip(t *testing.T) {
	rs := NewRangeSet()
	rs.Add(0, 1024)
	rs.Add(4096, 4096)

	data, err := json.Marshal(rs)
	if err != nil {
		t.Fatal(err)
	}

	expected := `[[0,1024],[4096,8192]]`
	if string(data) != expected {
		t.Fatalf("JSON = %s, want %s", data, expected)
	}

	rs2 := NewRangeSet()
	if err := json.Unmarshal(data, rs2); err != nil {
		t.Fatal(err)
	}
	if rs2.Len() != 2 {
		t.Fatalf("expected 2 intervals after unmarshal, got %d", rs2.Len())
	}
	ivs := rs2.Intervals()
	if ivs[0].Start != 0 || ivs[0].End != 1024 {
		t.Fatalf("first interval wrong: %v", ivs[0])
	}
	if ivs[1].Start != 4096 || ivs[1].End != 8192 {
		t.Fatalf("second interval wrong: %v", ivs[1])
	}
}

func TestJSONEmpty(t *testing.T) {
	rs := NewRangeSet()
	data, err := json.Marshal(rs)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "[]" {
		t.Fatalf("JSON = %s, want []", data)
	}

	rs2 := NewRangeSet()
	if err := json.Unmarshal(data, rs2); err != nil {
		t.Fatal(err)
	}
	if rs2.Len() != 0 {
		t.Fatalf("expected 0 intervals, got %d", rs2.Len())
	}
}

func TestSingleByte(t *testing.T) {
	rs := NewRangeSet()
	rs.Add(42, 1)
	if !rs.Contains(42, 1) {
		t.Error("expected to contain single byte")
	}
	if rs.Contains(41, 1) {
		t.Error("should not contain byte before")
	}
	if rs.Contains(43, 1) {
		t.Error("should not contain byte after")
	}
}

func TestString(t *testing.T) {
	rs := NewRangeSet()
	rs.Add(0, 10)
	s := rs.String()
	if s != "[[0,10]]" {
		t.Fatalf("String() = %s, want [[0,10]]", s)
	}
}
