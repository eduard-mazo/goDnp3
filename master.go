package godnp3

import "context"

// Master abstracts the DNP3 master runtime.
//
// Lifecycle: NewMaster → AddOutstation* → Start (connects TCP channels, sends
// startup integrity polls per association) → ... → Stop (closes channels,
// cancels associations).
//
// Implementations:
//   - stubMaster in master_stub.go (default; no C deps)
//   - ffiMaster in master_ffi.go  (build tag dnp3_ffi; wraps opendnp3)
type Master interface {
	// AddOutstation registers an outstation. Must be called before Start.
	// Re-adding the same ID replaces the prior config.
	AddOutstation(o OutstationConfig) error

	// RemoveOutstation removes an outstation and tears down its channel.
	// Safe to call while running.
	RemoveOutstation(id string) error

	// Start brings up all enabled outstation channels and begins polling.
	// The provided context cancels long-running setup. The Handler will
	// receive measurements asynchronously from internal goroutines.
	Start(ctx context.Context) error

	// Stop tears down all channels and waits for in-flight callbacks.
	Stop()

	// Status returns the current per-outstation status snapshot.
	Status() []Status

	// IntegrityPoll triggers an on-demand integrity poll for one outstation.
	// Returns immediately; the response arrives via Handler.OnMeasurement.
	IntegrityPoll(outstationID string) error

	// OperateBinary issues a DirectOperate CROB to an outstation, latching the
	// binary output at index on/off. Returns once dispatched (the async result is
	// delivered to opendnp3 logging).
	OperateBinary(outstationID string, index uint16, on bool) error

	// OperateAnalog issues a DirectOperate analog-output control to an outstation.
	OperateAnalog(outstationID string, index uint16, value float64) error
}

// NewMaster constructs a Master. The concrete type is selected by build tag.
// See newMaster in master_{stub,ffi}.go.
func NewMaster(h Handler) Master {
	return newMaster(h)
}
