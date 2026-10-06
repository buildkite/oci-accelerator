package main

import (
	"fmt"
	"io"

	"github.com/buildkite/oci-accelerator/internal/peering"
)

// writeAvailabilityMetrics appends availability families to a metrics scrape.
// They are present only while peering is enabled.
func writeAvailabilityMetrics(w io.Writer, stats peering.AvailabilityStats) {
	_, _ = fmt.Fprintf(w, `# HELP oci_accelerator_availability_hints Retained cache availability hints, local and remote.
# TYPE oci_accelerator_availability_hints gauge
oci_accelerator_availability_hints %d
# HELP oci_accelerator_availability_hint_evictions_total Hints evicted from the bounded directory to make room.
# TYPE oci_accelerator_availability_hint_evictions_total counter
oci_accelerator_availability_hint_evictions_total %d
# HELP oci_accelerator_availability_sends_total Availability requests sent to peers, by outcome.
# TYPE oci_accelerator_availability_sends_total counter
oci_accelerator_availability_sends_total{outcome="acknowledged"} %d
oci_accelerator_availability_sends_total{outcome="failed"} %d
# HELP oci_accelerator_availability_checkpoint_failures_total Failed writes of the availability snapshot.
# TYPE oci_accelerator_availability_checkpoint_failures_total counter
oci_accelerator_availability_checkpoint_failures_total %d
`, stats.Hints, stats.Evictions, stats.Acknowledged, stats.Failed, stats.CheckpointFailures)
}
