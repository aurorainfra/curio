package sealsupra

import (
	"go.opencensus.io/stats"
	"go.opencensus.io/stats/view"
	"go.opencensus.io/tag"
)

var (
	phaseKey, _      = tag.NewKey("phase")
	nvmeDeviceKey, _ = tag.NewKey("nvme_device")
	pre              = "sealsupra_"
)

// SupraSealMeasures groups all SupraSeal metrics.
var SupraSealMeasures = struct {
	PhaseLockCount     *stats.Int64Measure
	PhaseWaitingCount  *stats.Int64Measure
	PhaseAvgDuration   *stats.Float64Measure
	PhaseDurationSoFar *stats.Float64Measure

	// NVMe Health measures
	NVMeTemperature     *stats.Float64Measure
	NVMeAvailableSpare  *stats.Int64Measure
	NVMePercentageUsed  *stats.Int64Measure
	NVMePowerCycles     *stats.Int64Measure
	NVMePowerOnHours    *stats.Float64Measure
	NVMeUnsafeShutdowns *stats.Int64Measure
	NVMeMediaErrors     *stats.Int64Measure
	NVMeErrorLogEntries *stats.Int64Measure
	NVMeCriticalWarning *stats.Int64Measure

	NVMeBytesRead    *stats.Int64Measure
	NVMeBytesWritten *stats.Int64Measure
	NVMeReadIO       *stats.Int64Measure
	NVMeWriteIO      *stats.Int64Measure

	// NVMe namespace allocation (Identify Namespace) and trim
	NVMeNamespaceSize      *stats.Int64Measure
	NVMeNamespaceCapacity  *stats.Int64Measure
	NVMeNamespaceAllocated *stats.Int64Measure
	NVMeThinProvisioned    *stats.Int64Measure
	NVMeTrimmedBytes       *stats.Int64Measure
}{
	PhaseLockCount:     stats.Int64(pre+"phase_lock_count", "Number of active locks in each phase", stats.UnitDimensionless),
	PhaseWaitingCount:  stats.Int64(pre+"phase_waiting_count", "Number of goroutines waiting for a phase lock", stats.UnitDimensionless),
	PhaseAvgDuration:   stats.Float64(pre+"phase_avg_duration", "Average duration of each phase in seconds", stats.UnitSeconds),
	PhaseDurationSoFar: stats.Float64(pre+"phase_duration_so_far", "Duration of the phase so far in seconds", stats.UnitSeconds),

	// NVMe Health measures
	NVMeTemperature:     stats.Float64(pre+"nvme_temperature_celsius", "NVMe Temperature in Celsius", stats.UnitDimensionless),
	NVMeAvailableSpare:  stats.Int64(pre+"nvme_available_spare", "NVMe Available Spare", stats.UnitDimensionless),
	NVMePercentageUsed:  stats.Int64(pre+"nvme_percentage_used", "NVMe Percentage Used", stats.UnitDimensionless),
	NVMePowerCycles:     stats.Int64(pre+"nvme_power_cycles", "NVMe Power Cycles", stats.UnitDimensionless),
	NVMePowerOnHours:    stats.Float64(pre+"nvme_power_on_hours", "NVMe Power On Hours", stats.UnitDimensionless),
	NVMeUnsafeShutdowns: stats.Int64(pre+"nvme_unsafe_shutdowns", "NVMe Unsafe Shutdowns", stats.UnitDimensionless),
	NVMeMediaErrors:     stats.Int64(pre+"nvme_media_errors", "NVMe Media Errors", stats.UnitDimensionless),
	NVMeErrorLogEntries: stats.Int64(pre+"nvme_error_log_entries", "NVMe Error Log Entries", stats.UnitDimensionless),
	NVMeCriticalWarning: stats.Int64(pre+"nvme_critical_warning", "NVMe Critical Warning Flags", stats.UnitDimensionless),

	NVMeBytesRead:    stats.Int64(pre+"nvme_bytes_read", "NVMe Bytes Read", stats.UnitBytes),
	NVMeBytesWritten: stats.Int64(pre+"nvme_bytes_written", "NVMe Bytes Written", stats.UnitBytes),
	NVMeReadIO:       stats.Int64(pre+"nvme_read_io", "NVMe Read IOs", stats.UnitDimensionless),
	NVMeWriteIO:      stats.Int64(pre+"nvme_write_io", "NVMe Write IOs", stats.UnitDimensionless),

	NVMeNamespaceSize:      stats.Int64(pre+"nvme_namespace_size_bytes", "NVMe namespace size (NSZE)", stats.UnitBytes),
	NVMeNamespaceCapacity:  stats.Int64(pre+"nvme_namespace_capacity_bytes", "NVMe namespace capacity (NCAP)", stats.UnitBytes),
	NVMeNamespaceAllocated: stats.Int64(pre+"nvme_namespace_allocated_bytes", "NVMe namespace allocated bytes (NUSE); only tracks trim on drives which support it", stats.UnitBytes),
	NVMeThinProvisioned:    stats.Int64(pre+"nvme_thin_provisioned", "NVMe namespace reports thin provisioning (NSFEAT.THINP), 1=yes", stats.UnitDimensionless),
	NVMeTrimmedBytes:       stats.Int64(pre+"nvme_trimmed_bytes", "Bytes of layer NVMe storage trimmed (deallocated), summed over all devices", stats.UnitBytes),
}

// init registers the views for SupraSeal metrics.
func init() {
	err := view.Register(
		&view.View{
			Measure:     SupraSealMeasures.PhaseLockCount,
			Aggregation: view.LastValue(),
			TagKeys:     []tag.Key{phaseKey},
		},
		&view.View{
			Measure:     SupraSealMeasures.PhaseWaitingCount,
			Aggregation: view.LastValue(),
			TagKeys:     []tag.Key{phaseKey},
		},
		&view.View{
			Measure:     SupraSealMeasures.PhaseAvgDuration,
			Aggregation: view.LastValue(),
			TagKeys:     []tag.Key{phaseKey},
		},
		&view.View{
			Measure:     SupraSealMeasures.PhaseDurationSoFar,
			Aggregation: view.LastValue(),
			TagKeys:     []tag.Key{phaseKey},
		},
		// NVMe Health views
		&view.View{
			Measure:     SupraSealMeasures.NVMeTemperature,
			Aggregation: view.LastValue(),
			TagKeys:     []tag.Key{nvmeDeviceKey},
		},
		&view.View{
			Measure:     SupraSealMeasures.NVMeAvailableSpare,
			Aggregation: view.LastValue(),
			TagKeys:     []tag.Key{nvmeDeviceKey},
		},
		&view.View{
			Measure:     SupraSealMeasures.NVMePercentageUsed,
			Aggregation: view.LastValue(),
			TagKeys:     []tag.Key{nvmeDeviceKey},
		},
		&view.View{
			Measure:     SupraSealMeasures.NVMePowerCycles,
			Aggregation: view.LastValue(),
			TagKeys:     []tag.Key{nvmeDeviceKey},
		},
		&view.View{
			Measure:     SupraSealMeasures.NVMePowerOnHours,
			Aggregation: view.LastValue(),
			TagKeys:     []tag.Key{nvmeDeviceKey},
		},
		&view.View{
			Measure:     SupraSealMeasures.NVMeUnsafeShutdowns,
			Aggregation: view.LastValue(),
			TagKeys:     []tag.Key{nvmeDeviceKey},
		},
		&view.View{
			Measure:     SupraSealMeasures.NVMeMediaErrors,
			Aggregation: view.LastValue(),
			TagKeys:     []tag.Key{nvmeDeviceKey},
		},
		&view.View{
			Measure:     SupraSealMeasures.NVMeErrorLogEntries,
			Aggregation: view.LastValue(),
			TagKeys:     []tag.Key{nvmeDeviceKey},
		},
		&view.View{
			Measure:     SupraSealMeasures.NVMeCriticalWarning,
			Aggregation: view.LastValue(),
			TagKeys:     []tag.Key{nvmeDeviceKey},
		},
		&view.View{
			Measure:     SupraSealMeasures.NVMeBytesRead,
			Aggregation: view.Sum(),
			TagKeys:     []tag.Key{nvmeDeviceKey},
		},
		&view.View{
			Measure:     SupraSealMeasures.NVMeBytesWritten,
			Aggregation: view.Sum(),
			TagKeys:     []tag.Key{nvmeDeviceKey},
		},
		&view.View{
			Measure:     SupraSealMeasures.NVMeReadIO,
			Aggregation: view.Sum(),
			TagKeys:     []tag.Key{nvmeDeviceKey},
		},
		&view.View{
			Measure:     SupraSealMeasures.NVMeWriteIO,
			Aggregation: view.Sum(),
			TagKeys:     []tag.Key{nvmeDeviceKey},
		},
		&view.View{
			Measure:     SupraSealMeasures.NVMeReadIO,
			Aggregation: view.Sum(),
			TagKeys:     []tag.Key{nvmeDeviceKey},
		},
		&view.View{
			Measure:     SupraSealMeasures.NVMeWriteIO,
			Aggregation: view.Sum(),
			TagKeys:     []tag.Key{nvmeDeviceKey},
		},
		&view.View{
			Measure:     SupraSealMeasures.NVMeNamespaceSize,
			Aggregation: view.LastValue(),
			TagKeys:     []tag.Key{nvmeDeviceKey},
		},
		&view.View{
			Measure:     SupraSealMeasures.NVMeNamespaceCapacity,
			Aggregation: view.LastValue(),
			TagKeys:     []tag.Key{nvmeDeviceKey},
		},
		&view.View{
			Measure:     SupraSealMeasures.NVMeNamespaceAllocated,
			Aggregation: view.LastValue(),
			TagKeys:     []tag.Key{nvmeDeviceKey},
		},
		&view.View{
			Measure:     SupraSealMeasures.NVMeThinProvisioned,
			Aggregation: view.LastValue(),
			TagKeys:     []tag.Key{nvmeDeviceKey},
		},
		&view.View{
			Measure:     SupraSealMeasures.NVMeTrimmedBytes,
			Aggregation: view.Sum(),
		},
	)
	if err != nil {
		panic(err)
	}
}
