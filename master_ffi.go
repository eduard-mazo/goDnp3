//go:build dnp3_ffi

// CGO binding to opendnp3 (Apache 2.0) via the flat C shim in opendnp3_c.{h,cpp}.
//
// Layout:
//   - The cgo preamble forward-declares the //export Go callbacks and wires
//     them into an odc_callbacks vtable (odc_build_callbacks) handed to the
//     shim at manager creation.
//   - One shim odc_manager per process (the asio thread pool + log handler);
//     one odc_master (TCP-client channel + master association) per outstation.
//   - The void* ctx carried through every per-association callback is a
//     cgo.Handle to the owning *assocCtx, reinterpreted as a pointer in C via
//     odc_handle_to_ptr (keeps `go vet` happy — no unsafe.Pointer(uintptr)).
//   - The shim invokes callbacks from opendnp3 pool threads; we hand the
//     resulting dnp3.Measurement / OutstationStatus to the Handler directly.
//
// Build: go build -tags dnp3_ffi with CGO_CXXFLAGS/CGO_LDFLAGS pointing at the
// vendored opendnp3 — see the Makefile ffi targets.
package godnp3

/*
#include <stdlib.h>
#include <string.h>
#include <stdint.h>
#include "opendnp3_c.h"

// Exported Go callbacks (definitions emitted by cgo from the //export funcs).
extern void goOdcChannelState(void* ctx, int state);
extern void goOdcBinary(void* ctx, uint16_t index, int value, uint8_t flags, uint64_t ts_ms, int ts_quality, int read_type);
extern void goOdcDoubleBit(void* ctx, uint16_t index, int value, uint8_t flags, uint64_t ts_ms, int ts_quality, int read_type);
extern void goOdcBinaryOutputStatus(void* ctx, uint16_t index, int value, uint8_t flags, uint64_t ts_ms, int ts_quality, int read_type);
extern void goOdcCounter(void* ctx, uint16_t index, uint32_t value, uint8_t flags, uint64_t ts_ms, int ts_quality, int read_type);
extern void goOdcFrozenCounter(void* ctx, uint16_t index, uint32_t value, uint8_t flags, uint64_t ts_ms, int ts_quality, int read_type);
extern void goOdcAnalog(void* ctx, uint16_t index, double value, uint8_t flags, uint64_t ts_ms, int ts_quality, int read_type);
extern void goOdcAnalogOutputStatus(void* ctx, uint16_t index, double value, uint8_t flags, uint64_t ts_ms, int ts_quality, int read_type);
extern void goOdcOctetString(void* ctx, uint16_t index, uint8_t* data, size_t len, int read_type);
extern void goOdcLog(void* log_ctx, int level, char* msg);

// const-correctness shims: the cgo-generated prototypes are non-const, so wrap
// the two callbacks whose vtable fields take const pointers.
static void odc_wrap_octet(void* ctx, uint16_t index, const uint8_t* data, size_t len, int rt) {
    goOdcOctetString(ctx, index, (uint8_t*)data, len, rt);
}
static void odc_wrap_log(void* lctx, int level, const char* msg) {
    goOdcLog(lctx, level, (char*)msg);
}

static odc_callbacks odc_build_callbacks(void) {
    odc_callbacks c;
    memset(&c, 0, sizeof(c));
    c.on_channel_state        = goOdcChannelState;
    c.on_binary               = goOdcBinary;
    c.on_double_bit           = goOdcDoubleBit;
    c.on_binary_output_status = goOdcBinaryOutputStatus;
    c.on_counter              = goOdcCounter;
    c.on_frozen_counter       = goOdcFrozenCounter;
    c.on_analog               = goOdcAnalog;
    c.on_analog_output_status = goOdcAnalogOutputStatus;
    c.on_octet_string         = odc_wrap_octet;
    c.on_log                  = odc_wrap_log;
    return c;
}

// Reinterpret a cgo.Handle (an opaque integer index) as the void* ctx the shim
// carries. The shim never dereferences it; we recover the handle on callback.
static void* odc_handle_to_ptr(uintptr_t h) { return (void*)h; }
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/cgo"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

// ffiMaster is the opendnp3-backed Master.
type ffiMaster struct {
	h Handler

	// manager is this role's reference to the process-wide shared opendnp3
	// manager (see acquireManager); nil until Start, nil again after Stop.
	manager *C.odc_manager

	mu      sync.Mutex
	assocs  map[string]*assocCtx // outstation ID → context
	started atomic.Bool
}

// assocCtx is the per-outstation state pinned via cgo.Handle. The shim's ctx
// void* on every callback for this outstation reinterprets this handle.
//
// Only the cgo.Handle (an integer) is passed to C, never the struct, so the
// status mutex can live inline without violating cgo's pointer-passing rules.
type assocCtx struct {
	outstationID string
	master       *ffiMaster // for Handler dispatch
	cfg          OutstationConfig

	odc *C.odc_master // shim master (channel + association); nil until brought up

	handle cgo.Handle

	statusMu sync.RWMutex
	status   Status // mirrored from callbacks; read by Status()
}

// staticVariation is one (group, variation) "all objects" read used for static
// polling of outstations that don't flag events. The "with flags" variation of
// each group is used so per-point quality is reported.
type staticVariation struct{ group, variation uint8 }

var staticVariations = []staticVariation{
	{1, 2},  // binary input with flags
	{3, 2},  // double-bit binary with flags
	{10, 2}, // binary output status with flags
	{20, 1}, // counter 32-bit with flag
	{21, 1}, // frozen counter 32-bit with flag
	{30, 1}, // analog input 32-bit with flag
	{40, 1}, // analog output status 32-bit with flag
}

func newMaster(h Handler) Master {
	return &ffiMaster{
		h:      h,
		assocs: make(map[string]*assocCtx),
	}
}

// opendnp3 LogLevels bitfield (logging/LogLevels.h): EVENT=1 ERR=2 WARN=4 INFO=8 DBG=16.
// Production default drops INFO — opendnp3 logs a line per scan task at INFO,
// which floods journald/disk on an embedded gateway. When the operator runs the
// gateway at -log debug we raise to everything for diagnostics.
const (
	odcLogQuiet   int32 = 1 | 2 | 4 // EVENT|ERR|WARN
	odcLogVerbose int32 = -1        // all bits
)

// Process-wide opendnp3 manager (one asio thread pool + log sink) shared by both
// the master (AddTCPClient) and outstation (AddTCPServer) roles. Reference
// counted so the manager is created on the first role to start and torn down
// only when the last role stops. opendnp3 supports many channels per manager, so
// one manager serves any number of masters and the single outstation.
var (
	sharedMgrMu   sync.Mutex
	sharedMgr     *C.odc_manager
	sharedMgrRefs int
)

// acquireManager returns the shared manager, creating it on first use. Every
// successful call must be paired with exactly one releaseManager.
func acquireManager() (*C.odc_manager, error) {
	sharedMgrMu.Lock()
	defer sharedMgrMu.Unlock()
	if sharedMgr == nil {
		cbs := C.odc_build_callbacks()
		mask := odcLogQuiet
		if slog.Default().Enabled(context.Background(), slog.LevelDebug) {
			mask = odcLogVerbose
		}
		// concurrency 0 → shim auto-sizes; nil log ctx (lib logs route to slog).
		mgr := C.odc_manager_create(0, cbs, nil, C.int32_t(mask))
		if mgr == nil {
			return nil, errors.New("opendnp3: odc_manager_create failed")
		}
		sharedMgr = mgr
	}
	sharedMgrRefs++
	return sharedMgr, nil
}

// releaseManager drops one reference to the shared manager, destroying it (and
// its thread pool) when the last reference is released.
func releaseManager() {
	sharedMgrMu.Lock()
	defer sharedMgrMu.Unlock()
	if sharedMgrRefs == 0 {
		return
	}
	sharedMgrRefs--
	if sharedMgrRefs == 0 && sharedMgr != nil {
		C.odc_manager_destroy(sharedMgr)
		sharedMgr = nil
	}
}

func (m *ffiMaster) ensureManager() error {
	mgr, err := acquireManager()
	if err != nil {
		return err
	}
	m.manager = mgr
	return nil
}

func (m *ffiMaster) AddOutstation(o OutstationConfig) error {
	if m.started.Load() {
		// Adding outstations mid-run requires bringing up a channel against the
		// running manager; not supported yet.
		return errors.New("opendnp3: cannot AddOutstation after Start")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.assocs[o.ID]; exists {
		return fmt.Errorf("opendnp3: outstation %q already added", o.ID)
	}
	ctx := &assocCtx{outstationID: o.ID, master: m, cfg: o}
	ctx.status = Status{ID: o.ID, Label: o.Label, Addr: o.Addr()}
	ctx.handle = cgo.NewHandle(ctx)
	m.assocs[o.ID] = ctx
	return nil
}

func (m *ffiMaster) RemoveOutstation(id string) error {
	// Remove from the map (so Status no longer reports it) before destroying the
	// shim master: odc_master_destroy shuts the channel down synchronously, which
	// fires a CLOSED/SHUTDOWN state change → goOdcChannelState → Handler, and the
	// Handler may re-enter Status(). Releasing m.mu first avoids the deadlock.
	m.mu.Lock()
	ctx, ok := m.assocs[id]
	if ok {
		delete(m.assocs, id)
	}
	m.mu.Unlock()
	if !ok {
		return nil
	}
	if ctx.odc != nil {
		C.odc_master_destroy(ctx.odc)
		ctx.odc = nil
	}
	ctx.handle.Delete()
	return nil
}

func (m *ffiMaster) Start(_ context.Context) error {
	if m.started.Swap(true) {
		return errors.New("opendnp3: already started")
	}
	if err := m.ensureManager(); err != nil {
		return err
	}
	// Snapshot the assoc list under the lock, then release it before any shim
	// call. odc_manager_add_master / odc_master_enable can invoke the channel
	// listener synchronously, which calls back into Go and ultimately Status();
	// holding m.mu across that would deadlock.
	m.mu.Lock()
	list := make([]*assocCtx, 0, len(m.assocs))
	for _, ctx := range m.assocs {
		list = append(list, ctx)
	}
	m.mu.Unlock()
	for _, ctx := range list {
		if err := m.bringUp(ctx); err != nil {
			return err
		}
	}
	return nil
}

// bringUp adds the shim master for one outstation, registers its scans per the
// config, and enables it. Must NOT be called while holding m.mu.
func (m *ffiMaster) bringUp(ctx *assocCtx) error {
	o := ctx.cfg

	cHost := C.CString(o.Host)
	defer C.free(unsafe.Pointer(cHost))
	cID := C.CString(o.ID)
	defer C.free(unsafe.Pointer(cID))

	// Startup integrity class mask. If StartupIntegrity is off, no startup scan.
	// If on but no class flagged, default to all classes (DNP3 conformant).
	integ0, integ1, integ2, integ3 := o.IntegrityClass0, o.IntegrityClass1, o.IntegrityClass2, o.IntegrityClass3
	if !o.StartupIntegrity {
		integ0, integ1, integ2, integ3 = false, false, false, false
	} else if !integ0 && !integ1 && !integ2 && !integ3 {
		integ0, integ1, integ2, integ3 = true, true, true, true
	}

	var cfg C.odc_master_config
	cfg.host = cHost
	cfg.port = C.uint16_t(defaultPort(o.Port))
	cfg.master_address = C.uint16_t(o.MasterAddress)
	cfg.outstation_address = C.uint16_t(o.OutstationAddress)
	cfg.response_timeout_ms = C.uint32_t(defaultMs(o.ResponseTimeoutMs, 5000))
	cfg.keep_alive_ms = C.uint32_t(defaultMs(o.KeepAliveMs, 60000))
	cfg.disable_unsol_on_startup = boolCInt(o.DisableUnsolOnStartup)
	cfg.unsol_class1 = boolCInt(o.UnsolicitedEnabled && o.UnsolicitedClass1)
	cfg.unsol_class2 = boolCInt(o.UnsolicitedEnabled && o.UnsolicitedClass2)
	cfg.unsol_class3 = boolCInt(o.UnsolicitedEnabled && o.UnsolicitedClass3)
	cfg.startup_integrity_class0 = boolCInt(integ0)
	cfg.startup_integrity_class1 = boolCInt(integ1)
	cfg.startup_integrity_class2 = boolCInt(integ2)
	cfg.startup_integrity_class3 = boolCInt(integ3)

	mst := C.odc_manager_add_master(m.manager, cID, cfg, C.odc_handle_to_ptr(C.uintptr_t(ctx.handle)))
	if mst == nil {
		return fmt.Errorf("opendnp3: add master %q failed", o.ID)
	}
	ctx.odc = mst

	// If any scan/enable below fails, destroy the half-built master so it doesn't
	// leak inside the manager (its thread keeps running otherwise).
	ok := false
	defer func() {
		if !ok {
			C.odc_master_destroy(mst)
			ctx.odc = nil
		}
	}()

	// Periodic class scans.
	if o.IntegrityScanMs > 0 {
		if rc := C.odc_master_add_class_scan(mst, 1, 1, 1, 1, C.uint32_t(o.IntegrityScanMs)); rc != 0 {
			return fmt.Errorf("opendnp3: integrity scan for %q failed (rc=%d)", o.ID, int(rc))
		}
	}
	if o.Class1ScanMs > 0 {
		if rc := C.odc_master_add_class_scan(mst, 0, 1, 0, 0, C.uint32_t(o.Class1ScanMs)); rc != 0 {
			return fmt.Errorf("opendnp3: class1 scan for %q failed (rc=%d)", o.ID, int(rc))
		}
	}
	if o.Class2ScanMs > 0 {
		if rc := C.odc_master_add_class_scan(mst, 0, 0, 1, 0, C.uint32_t(o.Class2ScanMs)); rc != 0 {
			return fmt.Errorf("opendnp3: class2 scan for %q failed (rc=%d)", o.ID, int(rc))
		}
	}
	if o.Class3ScanMs > 0 {
		if rc := C.odc_master_add_class_scan(mst, 0, 0, 0, 1, C.uint32_t(o.Class3ScanMs)); rc != 0 {
			return fmt.Errorf("opendnp3: class3 scan for %q failed (rc=%d)", o.ID, int(rc))
		}
	}

	// Static (group-specific) all-objects polls.
	if o.StaticPollMs > 0 {
		for _, gv := range staticVariations {
			if rc := C.odc_master_add_all_objects_scan(mst, C.uint8_t(gv.group), C.uint8_t(gv.variation), C.uint32_t(o.StaticPollMs)); rc != 0 {
				return fmt.Errorf("opendnp3: static poll g%dv%d for %q failed (rc=%d)", gv.group, gv.variation, o.ID, int(rc))
			}
		}
	}

	if rc := C.odc_master_enable(mst); rc != 0 {
		return fmt.Errorf("opendnp3: enable master %q failed (rc=%d)", o.ID, int(rc))
	}
	ok = true
	return nil
}

func (m *ffiMaster) Stop() {
	if !m.started.Swap(false) {
		return
	}
	// Snapshot under the lock, then call into the shim without holding it:
	// odc_master_destroy fires synchronous state-change callbacks that re-enter
	// Status(), which would deadlock against a held m.mu.
	m.mu.Lock()
	list := make([]*assocCtx, 0, len(m.assocs))
	for _, ctx := range m.assocs {
		list = append(list, ctx)
	}
	m.mu.Unlock()

	for _, ctx := range list {
		if ctx.odc != nil {
			C.odc_master_destroy(ctx.odc)
			ctx.odc = nil
		}
	}

	// Release our reference to the shared manager. It is torn down only when the
	// last role (master or outstation) releases it, so if the outstation is still
	// up the manager stays alive — harmless, since our own masters are already
	// destroyed above. Released before deleting cgo.Handles so any last callback
	// during a real teardown still resolves its handle.
	if m.manager != nil {
		releaseManager()
		m.manager = nil
	}

	m.mu.Lock()
	for id, ctx := range m.assocs {
		ctx.handle.Delete()
		delete(m.assocs, id)
	}
	m.mu.Unlock()
}

func (m *ffiMaster) Status() []Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Status, 0, len(m.assocs))
	for _, ctx := range m.assocs {
		ctx.statusMu.RLock()
		out = append(out, ctx.status)
		ctx.statusMu.RUnlock()
	}
	return out
}

func (m *ffiMaster) IntegrityPoll(id string) error {
	m.mu.Lock()
	ctx, ok := m.assocs[id]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("opendnp3: unknown outstation %q", id)
	}
	if ctx.odc == nil {
		return fmt.Errorf("opendnp3: outstation %q not started", id)
	}
	if rc := C.odc_master_scan_classes(ctx.odc, 1, 1, 1, 1); rc != 0 {
		return fmt.Errorf("opendnp3: integrity poll for %q failed (rc=%d)", id, int(rc))
	}
	return nil
}

// operable resolves a started outstation association for a control operation.
func (m *ffiMaster) operable(id string) (*assocCtx, error) {
	m.mu.Lock()
	ctx, ok := m.assocs[id]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("opendnp3: unknown outstation %q", id)
	}
	if ctx.odc == nil {
		return nil, fmt.Errorf("opendnp3: outstation %q not started", id)
	}
	return ctx, nil
}

func (m *ffiMaster) OperateBinary(id string, index uint16, on bool) error {
	ctx, err := m.operable(id)
	if err != nil {
		return err
	}
	if rc := C.odc_master_operate_binary(ctx.odc, C.uint16_t(index), boolCInt(on)); rc != 0 {
		return fmt.Errorf("opendnp3: operate binary on %q failed (rc=%d)", id, int(rc))
	}
	return nil
}

func (m *ffiMaster) OperateAnalog(id string, index uint16, value float64) error {
	ctx, err := m.operable(id)
	if err != nil {
		return err
	}
	if rc := C.odc_master_operate_analog(ctx.odc, C.uint16_t(index), C.double(value)); rc != 0 {
		return fmt.Errorf("opendnp3: operate analog on %q failed (rc=%d)", id, int(rc))
	}
	return nil
}

// --- helpers ---------------------------------------------------------------

func defaultPort(p int) int {
	if p == 0 {
		return 20000
	}
	return p
}

func defaultMs(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

func boolCInt(b bool) C.int {
	if b {
		return 1
	}
	return 0
}

// makeMeasurement builds the gateway-facing Measurement from raw callback fields.
func makeMeasurement(ctx *assocCtx, pt PointType, idx uint16, flags uint8, tsMs uint64, tq int, rt int) Measurement {
	var t time.Time
	// opendnp3 TimestampQuality: 0=INVALID, 1=SYNCHRONIZED, 2=UNSYNCHRONIZED.
	// On INVALID the value carries no usable timestamp; fall back to local now.
	if tq == 0 {
		t = time.Now()
	} else {
		t = time.UnixMilli(int64(tsMs))
	}
	return Measurement{
		OutstationID: ctx.outstationID,
		PointType:    pt,
		Index:        idx,
		Time:         t,
		Quality:      Quality(flags),
		IsEvent:      rt == 1, // shim read_type: 1=event variation, 0=static/response
	}
}

// ctxFrom recovers the *assocCtx from the void* the shim carries. It returns nil
// (rather than panicking) if the handle is stale or unexpected — a callback can
// race teardown, and a panic here runs on an opendnp3 thread, which would abort
// the whole process. cgo.Handle.Value() panics on a deleted handle, so recover.
func ctxFrom(p unsafe.Pointer) (ctx *assocCtx) {
	defer func() { _ = recover() }()
	if c, ok := cgo.Handle(uintptr(p)).Value().(*assocCtx); ok {
		ctx = c
	}
	return
}

func deliver(ctx *assocCtx, m Measurement) {
	ctx.statusMu.Lock()
	ctx.status.MeasurementsRx++
	ctx.status.LastReadAt = time.Now()
	ctx.statusMu.Unlock()
	if ctx.master.h != nil {
		ctx.master.h.OnMeasurement(m)
	}
}

// --- //export funcs called by the shim vtable ------------------------------

//export goOdcChannelState
func goOdcChannelState(p unsafe.Pointer, state C.int) {
	ctx := ctxFrom(p)
	if ctx == nil {
		return
	}
	// opendnp3 ChannelState: 0=CLOSED 1=OPENING 2=OPEN 3=SHUTDOWN.
	ctx.statusMu.Lock()
	ctx.status.Connected = state == 2
	switch state {
	case 0:
		ctx.status.LastError = "closed"
	case 1:
		ctx.status.LastError = "opening"
	case 2:
		ctx.status.LastError = ""
	case 3:
		ctx.status.LastError = "shutdown"
	}
	snap := ctx.status
	ctx.statusMu.Unlock()
	if ctx.master.h != nil {
		ctx.master.h.OnStatusChange(snap)
	}
}

//export goOdcBinary
func goOdcBinary(p unsafe.Pointer, index C.uint16_t, value C.int, flags C.uint8_t, ts C.uint64_t, tq C.int, rt C.int) {
	ctx := ctxFrom(p)
	if ctx == nil {
		return
	}
	m := makeMeasurement(ctx, PointBinary, uint16(index), uint8(flags), uint64(ts), int(tq), int(rt))
	m.BoolValue = value != 0
	deliver(ctx, m)
}

//export goOdcDoubleBit
func goOdcDoubleBit(p unsafe.Pointer, index C.uint16_t, value C.int, flags C.uint8_t, ts C.uint64_t, tq C.int, rt C.int) {
	ctx := ctxFrom(p)
	if ctx == nil {
		return
	}
	m := makeMeasurement(ctx, PointDoubleBitBinary, uint16(index), uint8(flags), uint64(ts), int(tq), int(rt))
	m.DBBValue = DoubleBitState(value)
	deliver(ctx, m)
}

//export goOdcBinaryOutputStatus
func goOdcBinaryOutputStatus(p unsafe.Pointer, index C.uint16_t, value C.int, flags C.uint8_t, ts C.uint64_t, tq C.int, rt C.int) {
	ctx := ctxFrom(p)
	if ctx == nil {
		return
	}
	m := makeMeasurement(ctx, PointBinaryOutputStatus, uint16(index), uint8(flags), uint64(ts), int(tq), int(rt))
	m.BoolValue = value != 0
	deliver(ctx, m)
}

//export goOdcCounter
func goOdcCounter(p unsafe.Pointer, index C.uint16_t, value C.uint32_t, flags C.uint8_t, ts C.uint64_t, tq C.int, rt C.int) {
	ctx := ctxFrom(p)
	if ctx == nil {
		return
	}
	m := makeMeasurement(ctx, PointCounter, uint16(index), uint8(flags), uint64(ts), int(tq), int(rt))
	m.UintValue = uint32(value)
	deliver(ctx, m)
}

//export goOdcFrozenCounter
func goOdcFrozenCounter(p unsafe.Pointer, index C.uint16_t, value C.uint32_t, flags C.uint8_t, ts C.uint64_t, tq C.int, rt C.int) {
	ctx := ctxFrom(p)
	if ctx == nil {
		return
	}
	m := makeMeasurement(ctx, PointFrozenCounter, uint16(index), uint8(flags), uint64(ts), int(tq), int(rt))
	m.UintValue = uint32(value)
	deliver(ctx, m)
}

//export goOdcAnalog
func goOdcAnalog(p unsafe.Pointer, index C.uint16_t, value C.double, flags C.uint8_t, ts C.uint64_t, tq C.int, rt C.int) {
	ctx := ctxFrom(p)
	if ctx == nil {
		return
	}
	m := makeMeasurement(ctx, PointAnalog, uint16(index), uint8(flags), uint64(ts), int(tq), int(rt))
	m.FloatValue = float64(value)
	deliver(ctx, m)
}

//export goOdcAnalogOutputStatus
func goOdcAnalogOutputStatus(p unsafe.Pointer, index C.uint16_t, value C.double, flags C.uint8_t, ts C.uint64_t, tq C.int, rt C.int) {
	ctx := ctxFrom(p)
	if ctx == nil {
		return
	}
	m := makeMeasurement(ctx, PointAnalogOutputStatus, uint16(index), uint8(flags), uint64(ts), int(tq), int(rt))
	m.FloatValue = float64(value)
	deliver(ctx, m)
}

//export goOdcOctetString
func goOdcOctetString(p unsafe.Pointer, index C.uint16_t, data *C.uint8_t, length C.size_t, rt C.int) {
	ctx := ctxFrom(p)
	if ctx == nil {
		return
	}
	m := makeMeasurement(ctx, PointOctetString, uint16(index), 0, uint64(time.Now().UnixMilli()), 1, int(rt))
	if length > 0 {
		m.BytesValue = C.GoBytes(unsafe.Pointer(data), C.int(length))
	}
	deliver(ctx, m)
}

//export goOdcLog
func goOdcLog(_ unsafe.Pointer, level C.int, msg *C.char) {
	// opendnp3 stack logs go to slog only (parity with the old binding); the
	// Handler.OnLog channel is reserved for binding-level messages.
	s := C.GoString(msg)
	switch level {
	case 0:
		slog.Error("dnp3-lib: " + s)
	case 1:
		slog.Warn("dnp3-lib: " + s)
	default:
		slog.Info("dnp3-lib: " + s)
	}
}
