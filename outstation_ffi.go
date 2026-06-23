//go:build dnp3_ffi

// CGO binding to the opendnp3 outstation server via the flat C shim in
// opendnp3_c.{h,cpp}. The outstation hangs off the same process-wide manager as
// the masters (see acquireManager in master_ffi.go), so one asio thread pool and
// one log sink serve both roles.
package godnp3

/*
#include <stdlib.h>
#include <stdint.h>
#include "opendnp3_c.h"

// Exported Go callback (definition emitted by cgo from the //export func).
extern void goOdcOutstationChannelState(void* ctx, int state);

static odc_outstation_callbacks odc_build_outstation_callbacks(void) {
    odc_outstation_callbacks c;
    c.on_channel_state = goOdcOutstationChannelState;
    return c;
}

// Reinterpret a cgo.Handle as the void* ctx the shim carries. Defined per-file
// because cgo preamble statics are file-local (master_ffi.go has its own copy).
static void* odc_os_handle_to_ptr(uintptr_t h) { return (void*)h; }
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"runtime/cgo"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

// ffiOutstation is the opendnp3-backed Outstation server.
type ffiOutstation struct {
	cfg   ServerConfig
	sizes DBSizes
	h     Handler

	manager *C.odc_manager    // shared-manager reference; nil outside Start..Stop
	os      *C.odc_outstation // shim outstation; nil until Start
	handle  cgo.Handle        // pins this *ffiOutstation across the C boundary

	started atomic.Bool
	updates atomic.Int64

	statusMu sync.RWMutex
	status   Status // mirrored from the channel-state callback
}

func newOutstation(cfg ServerConfig, sizes DBSizes, h Handler) Outstation {
	o := &ffiOutstation{cfg: cfg, sizes: sizes, h: h}
	o.status = Status{ID: cfg.ServerID(), Label: cfg.Label, Addr: cfg.Addr()}
	return o
}

func (o *ffiOutstation) Start(_ context.Context) error {
	if o.started.Swap(true) {
		return errors.New("opendnp3 outstation: already started")
	}

	mgr, err := acquireManager()
	if err != nil {
		o.started.Store(false)
		return err
	}
	o.manager = mgr
	o.handle = cgo.NewHandle(o)

	// fail rolls back everything brought up so far, in reverse order, and resets
	// to the not-started state so a later Stop is a no-op.
	fail := func(err error) error {
		if o.os != nil {
			C.odc_outstation_destroy(o.os)
			o.os = nil
		}
		o.handle.Delete()
		releaseManager()
		o.manager = nil
		o.started.Store(false)
		return err
	}

	cID := C.CString(o.cfg.ServerID())
	defer C.free(unsafe.Pointer(cID))
	cHost := C.CString(o.cfg.Bind())
	defer C.free(unsafe.Pointer(cHost))

	var ccfg C.odc_outstation_config
	ccfg.bind_host = cHost
	ccfg.port = C.uint16_t(o.cfg.ServerPort())
	ccfg.local_address = C.uint16_t(o.cfg.LocalAddress)
	ccfg.master_address = C.uint16_t(o.cfg.MasterAddress)
	ccfg.allow_unsolicited = boolCInt(o.cfg.AllowUnsolicited)
	ccfg.event_buffer_size = C.uint16_t(o.cfg.EventBufferSize) // 0 → shim default 100
	ccfg.sizes.binary = C.uint16_t(o.sizes.Binary)
	ccfg.sizes.double_bit = C.uint16_t(o.sizes.DoubleBit)
	ccfg.sizes.analog = C.uint16_t(o.sizes.Analog)
	ccfg.sizes.counter = C.uint16_t(o.sizes.Counter)
	ccfg.sizes.frozen_counter = C.uint16_t(o.sizes.FrozenCounter)
	ccfg.sizes.binary_output_status = C.uint16_t(o.sizes.BinaryOutputStatus)
	ccfg.sizes.analog_output_status = C.uint16_t(o.sizes.AnalogOutputStatus)
	ccfg.sizes.octet_string = C.uint16_t(o.sizes.OctetString)

	cbs := C.odc_build_outstation_callbacks()
	os := C.odc_manager_add_outstation(o.manager, cID, ccfg, cbs,
		C.odc_os_handle_to_ptr(C.uintptr_t(o.handle)))
	if os == nil {
		return fail(fmt.Errorf("opendnp3 outstation: bind/add failed on %s", o.cfg.Addr()))
	}
	o.os = os

	if rc := C.odc_outstation_enable(os); rc != 0 {
		return fail(fmt.Errorf("opendnp3 outstation: enable failed (rc=%d)", int(rc)))
	}
	return nil
}

func (o *ffiOutstation) Stop() {
	if !o.started.Swap(false) {
		return
	}
	// Destroy the outstation (shuts its server channel, may fire a final
	// channel-state callback synchronously) before deleting the handle, so that
	// callback still resolves. Then drop our shared-manager reference.
	if o.os != nil {
		C.odc_outstation_destroy(o.os)
		o.os = nil
	}
	if o.manager != nil {
		releaseManager()
		o.manager = nil
	}
	o.handle.Delete()
}

func (o *ffiOutstation) Update(s Measurement) {
	if !o.started.Load() || o.os == nil {
		return
	}
	idx := C.uint16_t(s.Index)
	flags := C.uint8_t(s.Quality)
	var ts C.uint64_t
	if !s.Time.IsZero() {
		ts = C.uint64_t(s.Time.UnixMilli())
	}

	var rc C.int
	switch s.PointType {
	case PointBinary:
		rc = C.odc_outstation_update_binary(o.os, idx, boolCInt(s.BoolValue), flags, ts)
	case PointDoubleBitBinary:
		rc = C.odc_outstation_update_double_bit(o.os, idx, C.int(s.DBBValue), flags, ts)
	case PointBinaryOutputStatus:
		rc = C.odc_outstation_update_binary_output_status(o.os, idx, boolCInt(s.BoolValue), flags, ts)
	case PointCounter, PointFrozenCounter:
		// opendnp3 has no direct frozen-counter setter; serve it as a counter.
		rc = C.odc_outstation_update_counter(o.os, idx, C.uint32_t(s.UintValue), flags, ts)
	case PointAnalog:
		rc = C.odc_outstation_update_analog(o.os, idx, C.double(s.FloatValue), flags, ts)
	case PointAnalogOutputStatus:
		rc = C.odc_outstation_update_analog_output_status(o.os, idx, C.double(s.FloatValue), flags, ts)
	case PointOctetString:
		if len(s.BytesValue) == 0 {
			return
		}
		rc = C.odc_outstation_update_octet_string(o.os, idx,
			(*C.uint8_t)(unsafe.Pointer(&s.BytesValue[0])), C.size_t(len(s.BytesValue)))
	default:
		return
	}
	if rc != 0 {
		return // dropped (e.g. index outside the configured database size)
	}

	o.statusMu.Lock()
	o.status.MeasurementsRx = o.updates.Add(1)
	o.status.LastReadAt = time.Now()
	o.statusMu.Unlock()
}

func (o *ffiOutstation) Status() Status {
	o.statusMu.RLock()
	defer o.statusMu.RUnlock()
	return o.status
}

// outstationFrom recovers the *ffiOutstation from the void* the shim carries,
// returning nil on a stale/deleted handle rather than panicking — the callback
// can race teardown and a panic on an opendnp3 thread would abort the process.
func outstationFrom(p unsafe.Pointer) (o *ffiOutstation) {
	defer func() { _ = recover() }()
	if v, ok := cgo.Handle(uintptr(p)).Value().(*ffiOutstation); ok {
		o = v
	}
	return
}

//export goOdcOutstationChannelState
func goOdcOutstationChannelState(p unsafe.Pointer, state C.int) {
	o := outstationFrom(p)
	if o == nil {
		return
	}
	// opendnp3 ChannelState: 0=CLOSED 1=OPENING 2=OPEN 3=SHUTDOWN. For a TCP
	// server, OPEN means a master (SCADA) is connected.
	o.statusMu.Lock()
	o.status.Connected = state == 2
	switch state {
	case 0:
		o.status.LastError = "no master connected"
	case 1:
		o.status.LastError = "accepting"
	case 2:
		o.status.LastError = ""
	case 3:
		o.status.LastError = "shutdown"
	}
	snap := o.status
	o.statusMu.Unlock()
	if o.h != nil {
		o.h.OnStatusChange(snap)
	}
}
