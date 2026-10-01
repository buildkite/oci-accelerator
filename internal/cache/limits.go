package cache

import "time"

// Limits bounds a Store. Every field must be positive.
type Limits struct {
	MaxBytes    int64         // total charge of committed entries, active fills, and pending cleanup.
	MaxFills    int           // concurrent active fills.
	MaxEntries  int           // entry slots for committed entries, active fills, and pending cleanup.
	FillTimeout time.Duration // lifetime of a fill from Begin.
}

func (l Limits) valid() bool {
	return l.MaxBytes > 0 && l.MaxFills > 0 && l.MaxEntries > 0 && l.FillTimeout > 0
}
