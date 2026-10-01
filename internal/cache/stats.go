package cache

// Stats is a point-in-time snapshot of a Store's accounting counters.
type Stats struct {
	Bytes, Reserved                                 int64
	Entries, Fills, Pending                         int // Pending counts staging key claims: they consume a slot before a Fill exists.
	Evictions                                       int64
	Readers                                         int
	FillSuccess, FillFailure, FillAbort, FillExpiry int64
	Admissions                                      [AdmissionReasonCount]int64
	CleanupBytes                                    int64 // Included in Bytes until removal succeeds.
	CleanupSlots                                    int   // Charged against MaxEntries.
}
