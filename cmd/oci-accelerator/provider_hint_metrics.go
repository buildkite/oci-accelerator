package main

import (
	"fmt"
	"io"

	"github.com/buildkite/oci-accelerator/internal/peering"
)

// writeProviderHintMetrics appends provider hint families to a metrics scrape.
// They are present only while peering is enabled.
func writeProviderHintMetrics(w io.Writer, stats peering.ProviderHintStats) {
	_, _ = fmt.Fprintf(w, `# HELP oci_accelerator_provider_hints Retained provider hints, local and remote.
# TYPE oci_accelerator_provider_hints gauge
oci_accelerator_provider_hints %d
# HELP oci_accelerator_provider_hint_evictions_total Hints evicted from the bounded directory to make room.
# TYPE oci_accelerator_provider_hint_evictions_total counter
oci_accelerator_provider_hint_evictions_total %d
# HELP oci_accelerator_announcements_total Announcements sent to peers, by outcome.
# TYPE oci_accelerator_announcements_total counter
oci_accelerator_announcements_total{outcome="acknowledged"} %d
oci_accelerator_announcements_total{outcome="failed"} %d
# HELP oci_accelerator_provider_hint_checkpoint_failures_total Failed writes of the provider hint snapshot.
# TYPE oci_accelerator_provider_hint_checkpoint_failures_total counter
oci_accelerator_provider_hint_checkpoint_failures_total %d
`, stats.Hints, stats.Evictions, stats.Acknowledged, stats.Failed, stats.CheckpointFailures)
}
