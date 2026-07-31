package model

// HistoryLen is how many samples of trend data each sparkline keeps. Fixed
// capacity is the point: a session left running overnight uses exactly as much
// memory as one opened a second ago, and there is nothing to prune.
const HistoryLen = 60

// Ring is a fixed-capacity circular buffer of float64 readings. The zero value
// is ready to use.
type Ring struct {
	buf   [HistoryLen]float64
	start int // index of the oldest element
	n     int // number of elements held, saturating at HistoryLen
}

// Push appends v, discarding the oldest reading once the buffer is full.
func (r *Ring) Push(v float64) {
	if r.n < HistoryLen {
		r.buf[(r.start+r.n)%HistoryLen] = v
		r.n++
		return
	}
	r.buf[r.start] = v
	r.start = (r.start + 1) % HistoryLen
}

// Len reports how many readings are held.
func (r *Ring) Len() int { return r.n }

// Values returns the readings in chronological order, oldest first.
func (r *Ring) Values() []float64 {
	out := make([]float64, r.n)
	for i := 0; i < r.n; i++ {
		out[i] = r.buf[(r.start+i)%HistoryLen]
	}
	return out
}

// Last returns the most recent reading, or false if none has been pushed.
func (r *Ring) Last() (float64, bool) {
	if r.n == 0 {
		return 0, false
	}
	return r.buf[(r.start+r.n-1)%HistoryLen], true
}
