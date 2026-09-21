package cache

import "time"

type Limits struct {
	MaxBytes    int64
	MaxFills    int
	MaxEntries  int
	FillTimeout time.Duration
}

func (l Limits) valid() bool {
	return l.MaxBytes > 0 && l.MaxFills > 0 && l.MaxEntries > 0 && l.FillTimeout > 0
}
