package cache

import "fmt"

type AdmissionReason uint8

const (
	AdmissionAccepted AdmissionReason = iota
	AdmissionInvalid
	AdmissionOversized
	AdmissionExisting
	AdmissionFillLimit
	AdmissionCapacity
	AdmissionClosed
	AdmissionIOError
	AdmissionReasonCount
)

func (r AdmissionReason) String() string {
	names := [...]string{"accepted", "invalid", "oversized", "existing", "fill_limit", "capacity", "closed", "io_error"}
	if int(r) >= len(names) {
		return fmt.Sprintf("unknown(%d)", uint8(r))
	}
	return names[r]
}

type AdmissionError struct {
	Reason AdmissionReason
	Cause  error
}

func (e *AdmissionError) Error() string {
	message := "cache admission refused: " + e.Reason.String()
	if e.Cause != nil {
		message += ": " + e.Cause.Error()
	}
	return message
}
func (e *AdmissionError) Unwrap() error        { return e.Cause }
func (e *AdmissionError) Is(target error) bool { return target == ErrBypass }

func (s *Store) refuse(reason AdmissionReason, cause error) error {
	s.mu.Lock()
	s.admissions[reason]++
	s.mu.Unlock()
	return &AdmissionError{Reason: reason, Cause: cause}
}
