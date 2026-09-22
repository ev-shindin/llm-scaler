package throughput

import (
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/itl"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/shape"
)

// The shape and ITL signals live in internal/signals (engine-structure
// proposal, stage 1). These names keep this package's vocabulary, so the
// analyzer and its tests read as they did. They go with stage 3, which cuts
// the analyzer surface: new code imports signals/* directly and does not add
// to this file.
type (
	// WorkloadShape is shape.Shape.
	WorkloadShape = shape.Shape
	// ShapeTracker is shape.Tracker.
	ShapeTracker = shape.Tracker
	// ObservationWindow is itl.Window.
	ObservationWindow = itl.Window
	// ITLModel is itl.Model.
	ITLModel = itl.Model
)

const (
	// DefaultShapeChangeTolerance is shape.DefaultChangeTolerance.
	DefaultShapeChangeTolerance = shape.DefaultChangeTolerance
	// DefaultWindowMaxSize is itl.DefaultWindowMaxSize.
	DefaultWindowMaxSize = itl.DefaultWindowMaxSize
	// DefaultObservationMaxAge is itl.DefaultObservationMaxAge.
	DefaultObservationMaxAge = itl.DefaultObservationMaxAge
	// DefaultMinSamples is itl.DefaultMinSamples.
	DefaultMinSamples = itl.DefaultMinSamples
	// DefaultMinKSpread is itl.DefaultMinKSpread.
	DefaultMinKSpread = itl.DefaultMinKSpread
	// DefaultMinObservableK is itl.DefaultMinObservableK.
	DefaultMinObservableK = itl.DefaultMinObservableK
	// DefaultMaxObservableK is itl.DefaultMaxObservableK.
	DefaultMaxObservableK = itl.DefaultMaxObservableK
	// DefaultKSat is itl.DefaultKSat.
	DefaultKSat = itl.DefaultKSat
)
