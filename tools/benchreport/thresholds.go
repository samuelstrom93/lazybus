package main

// Units of Result.Unit.
const (
	unitNs    = "ns"
	unitBytes = "bytes"
	unitCount = "count"
)

const (
	kib = 1024
	mib = 1024 * kib
)

// thresholds is the one table of regression thresholds. A metric fails the
// gate only when its median grows by the ratio AND by the absolute floor;
// the time floors sit at 5–8% of the medians measured on framen and at
// least 3× run-to-run noise (docs/releasing.md lists the measurements). A
// metric missing here never fails as a regression.
var thresholds = map[string]Threshold{
	"StartupFirstFrame/ns_per_op": timeT(20e3),  // 20 µs
	"LoadLargeList/ns_per_op":     timeT(4e6),   // 4 ms
	"ViewLargeList/ns_per_op":     timeT(150e3), // 150 µs
	"NavStepLargeList/ns_per_op":  timeT(150e3), // 150 µs
	"RepairFlow/ns_per_op":        timeT(30e3),  // 30 µs

	"StartupFirstFrame/bytes_per_op": bytesT(32 * kib),
	"LoadLargeList/bytes_per_op":     bytesT(16 * mib),
	"ViewLargeList/bytes_per_op":     bytesT(32 * kib),
	"NavStepLargeList/bytes_per_op":  bytesT(32 * kib),
	"RepairFlow/bytes_per_op":        bytesT(32 * kib),

	"StartupFirstFrame/allocs_per_op": allocsT,
	"LoadLargeList/allocs_per_op":     allocsT,
	"ViewLargeList/allocs_per_op":     allocsT,
	"NavStepLargeList/allocs_per_op":  allocsT,
	"RepairFlow/allocs_per_op":        allocsT,

	// 10k loaded rows retain ~3.7 MiB, so an 8 MiB floor could never fire.
	"LoadLargeList/heap_bytes": {MaxRegressionRatio: 1.20, MinAbsoluteRegression: 1 * mib},

	binaryMetric: {MaxRegressionRatio: 1.10, MinAbsoluteRegression: 256 * kib},
}

func timeT(floorNs float64) Threshold {
	return Threshold{MaxRegressionRatio: 1.15, MinAbsoluteRegression: floorNs}
}

func bytesT(floor float64) Threshold {
	return Threshold{MaxRegressionRatio: 1.20, MinAbsoluteRegression: floor}
}

var allocsT = Threshold{MaxRegressionRatio: 1.20, MinAbsoluteRegression: 100}

// binaryMetric is the size of the stripped lazybus binary.
const binaryMetric = "binary/size_bytes"
