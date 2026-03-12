package download

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAddSingle(t *testing.T) {
	t.Parallel()
	rs := NewRangeSet()
	rs.Add(0, 100)
	require.Equal(t, 1, rs.Len())
	iv := rs.Intervals()[0]
	assert.Equal(t, int64(0), iv.Start)
	assert.Equal(t, int64(100), iv.End)
}

func TestAddNonOverlapping(t *testing.T) {
	t.Parallel()
	rs := NewRangeSet()
	rs.Add(0, 100)
	rs.Add(200, 100) // [0,100) [200,300)
	assert.Equal(t, 2, rs.Len())
}

func TestAddAdjacent(t *testing.T) {
	t.Parallel()
	rs := NewRangeSet()
	rs.Add(0, 100)
	rs.Add(100, 100) // should merge into [0,200)
	require.Equal(t, 1, rs.Len())
	iv := rs.Intervals()[0]
	assert.Equal(t, int64(0), iv.Start)
	assert.Equal(t, int64(200), iv.End)
}

func TestAddOverlapping(t *testing.T) {
	t.Parallel()
	rs := NewRangeSet()
	rs.Add(0, 100)
	rs.Add(50, 100) // overlap → [0,150)
	require.Equal(t, 1, rs.Len())
	iv := rs.Intervals()[0]
	assert.Equal(t, int64(0), iv.Start)
	assert.Equal(t, int64(150), iv.End)
}

func TestAddMergeMultiple(t *testing.T) {
	t.Parallel()
	rs := NewRangeSet()
	rs.Add(0, 100)
	rs.Add(200, 100)
	rs.Add(400, 100)
	// Now bridge [0-100) and [200-300) and [400-500)
	rs.Add(50, 450) // should merge all into [0,500)
	require.Equal(t, 1, rs.Len())
	iv := rs.Intervals()[0]
	assert.Equal(t, int64(0), iv.Start)
	assert.Equal(t, int64(500), iv.End)
}

func TestAddZeroLength(t *testing.T) {
	t.Parallel()
	rs := NewRangeSet()
	rs.Add(0, 0)
	assert.Equal(t, 0, rs.Len())
}

func TestAddNegativeLength(t *testing.T) {
	t.Parallel()
	rs := NewRangeSet()
	rs.Add(0, -5)
	assert.Equal(t, 0, rs.Len())
}

func TestAddInsertBefore(t *testing.T) {
	t.Parallel()
	rs := NewRangeSet()
	rs.Add(200, 100)
	rs.Add(0, 50) // insert before existing
	require.Equal(t, 2, rs.Len())
	ivs := rs.Intervals()
	assert.Equal(t, int64(0), ivs[0].Start)
	assert.Equal(t, int64(50), ivs[0].End)
	assert.Equal(t, int64(200), ivs[1].Start)
	assert.Equal(t, int64(300), ivs[1].End)
}

func TestContains(t *testing.T) {
	t.Parallel()
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
		assert.Equal(t, tc.want, rs.Contains(tc.off, tc.len),
			"Contains(%d, %d)", tc.off, tc.len)
	}
}

func TestIsComplete(t *testing.T) {
	t.Parallel()
	rs := NewRangeSet()
	rs.Add(0, 100)

	assert.True(t, rs.IsComplete(100), "totalSize=100")
	assert.True(t, rs.IsComplete(50), "totalSize=50")
	assert.False(t, rs.IsComplete(101), "totalSize=101")
	assert.True(t, rs.IsComplete(0), "totalSize=0")
}

func TestGaps(t *testing.T) {
	t.Parallel()
	rs := NewRangeSet()
	rs.Add(10, 20) // [10,30)
	rs.Add(50, 10) // [50,60)

	gaps := rs.Gaps(100)
	expected := []Interval{
		{0, 10},
		{30, 50},
		{60, 100},
	}
	require.Len(t, gaps, len(expected))
	for i, g := range gaps {
		assert.Equal(t, expected[i], g, "gap[%d]", i)
	}
}

func TestGapsComplete(t *testing.T) {
	t.Parallel()
	rs := NewRangeSet()
	rs.Add(0, 100)
	gaps := rs.Gaps(100)
	assert.Empty(t, gaps)
}

func TestGapsEmpty(t *testing.T) {
	t.Parallel()
	rs := NewRangeSet()
	gaps := rs.Gaps(100)
	require.Len(t, gaps, 1)
	assert.Equal(t, int64(0), gaps[0].Start)
	assert.Equal(t, int64(100), gaps[0].End)
}

func TestJSONRoundTrip(t *testing.T) {
	t.Parallel()
	rs := NewRangeSet()
	rs.Add(0, 1024)
	rs.Add(4096, 4096)

	data, err := json.Marshal(rs)
	require.NoError(t, err)
	assert.Equal(t, `[[0,1024],[4096,8192]]`, string(data))

	rs2 := NewRangeSet()
	require.NoError(t, json.Unmarshal(data, rs2))
	require.Equal(t, 2, rs2.Len())
	ivs := rs2.Intervals()
	assert.Equal(t, int64(0), ivs[0].Start)
	assert.Equal(t, int64(1024), ivs[0].End)
	assert.Equal(t, int64(4096), ivs[1].Start)
	assert.Equal(t, int64(8192), ivs[1].End)
}

func TestJSONEmpty(t *testing.T) {
	t.Parallel()
	rs := NewRangeSet()
	data, err := json.Marshal(rs)
	require.NoError(t, err)
	assert.Equal(t, "[]", string(data))

	rs2 := NewRangeSet()
	require.NoError(t, json.Unmarshal(data, rs2))
	assert.Equal(t, 0, rs2.Len())
}

func TestSingleByte(t *testing.T) {
	t.Parallel()
	rs := NewRangeSet()
	rs.Add(42, 1)
	assert.True(t, rs.Contains(42, 1), "should contain single byte")
	assert.False(t, rs.Contains(41, 1), "should not contain byte before")
	assert.False(t, rs.Contains(43, 1), "should not contain byte after")
}

func TestString(t *testing.T) {
	t.Parallel()
	rs := NewRangeSet()
	rs.Add(0, 10)
	assert.Equal(t, "[[0,10]]", rs.String())
}
