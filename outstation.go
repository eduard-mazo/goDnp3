package godnp3

import "context"

// Outstation is the DNP3 outstation server: it serves point values northbound to
// a SCADA master over DNP3.
//
// Unlike Master, an Outstation is a sink: callers push values into it via Update
// rather than it pushing measurements to a Handler. The Handler it is given is
// used only for status changes (master connect/disconnect) and log lines.
//
// Lifecycle: NewOutstation → Start (binds the TCP server, begins serving) →
// Update… (any goroutine) → Stop (tears down the server channel).
//
// Implementations:
//   - stubOutstation in outstation_stub.go (default; no C deps)
//   - ffiOutstation in outstation_ffi.go  (build tag dnp3_ffi; wraps opendnp3)
type Outstation interface {
	// Start brings up the server channel and enables the outstation.
	Start(ctx context.Context) error

	// Stop tears down the server channel and releases the shared manager ref.
	Stop()

	// Update pushes one measurement into the served database. The sample's
	// PointType/Index address a point in the outstation database (not the field
	// source), and the value carried per PointType is served as-is — callers pass
	// the engineering-scaled value and the quality they want SCADA to see. Safe to
	// call from any goroutine; a no-op before Start / after Stop.
	Update(s Measurement)

	// Status returns the current server status snapshot (one server, so a single
	// Status rather than the Master's slice). Connected reflects whether a SCADA
	// master is currently connected.
	Status() Status
}

// NewOutstation constructs an Outstation. The concrete type is selected by build
// tag (see newOutstation in outstation_{stub,ffi}.go).
func NewOutstation(cfg ServerConfig, sizes DBSizes, h Handler) Outstation {
	return newOutstation(cfg, sizes, h)
}
