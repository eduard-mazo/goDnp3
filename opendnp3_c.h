/*
 * opendnp3_c.h — flat C API over the opendnp3 (Apache 2.0) C++ master stack.
 *
 * This shim is the only place opendnp3's C++ types are touched. It exposes a
 * minimal, cgo-friendly C surface that mirrors exactly what the Go binding in
 * master_ffi.go needs: create a manager (thread pool + logger), add one
 * TCP-client master per outstation, register periodic class / all-objects
 * scans, trigger on-demand scans, and receive measurement + channel-state +
 * log callbacks.
 *
 * Callbacks are delivered through a function-pointer vtable (odc_callbacks)
 * that the Go side fills with the addresses of its //export functions. The
 * shim never references any Go/cgo symbol directly, so it compiles standalone
 * against the vendored headers (g++ -c) and stays decoupled from cgo.
 *
 * Enum value conventions passed across the boundary (raw opendnp3 values):
 *   channel state : 0=CLOSED 1=OPENING 2=OPEN 3=SHUTDOWN   (opendnp3::ChannelState)
 *   ts_quality    : 0=INVALID 1=SYNCHRONIZED 2=UNSYNCHRONIZED (opendnp3::TimestampQuality)
 *   read_type     : 0=static/response value, 1=event value   (HeaderInfo.isEventVariation)
 *   log level     : 0=error 1=warn 2=info 3=debug/other
 *   double-bit    : 0=intermediate 1=off 2=on 3=indeterminate (opendnp3::DoubleBit)
 */
#ifndef OPENDNP3_C_H
#define OPENDNP3_C_H

#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

/* Opaque handles owned by the shim. */
typedef struct odc_manager odc_manager;
typedef struct odc_master odc_master;

/*
 * Callback vtable. The shim invokes these from opendnp3 thread-pool threads.
 *
 * `ctx` on the per-association callbacks is the opaque pointer passed to
 * odc_manager_add_master (the Go cgo.Handle of the outstation). `log_ctx` on
 * on_log is the pointer passed to odc_manager_create.
 *
 * Any field may be NULL; the shim null-checks before calling.
 */
typedef struct {
    void (*on_channel_state)(void* ctx, int state);

    void (*on_binary)(void* ctx, uint16_t index, int value, uint8_t flags,
                      uint64_t ts_ms, int ts_quality, int read_type);
    void (*on_double_bit)(void* ctx, uint16_t index, int value, uint8_t flags,
                          uint64_t ts_ms, int ts_quality, int read_type);
    void (*on_binary_output_status)(void* ctx, uint16_t index, int value, uint8_t flags,
                                    uint64_t ts_ms, int ts_quality, int read_type);
    void (*on_counter)(void* ctx, uint16_t index, uint32_t value, uint8_t flags,
                       uint64_t ts_ms, int ts_quality, int read_type);
    void (*on_frozen_counter)(void* ctx, uint16_t index, uint32_t value, uint8_t flags,
                              uint64_t ts_ms, int ts_quality, int read_type);
    void (*on_analog)(void* ctx, uint16_t index, double value, uint8_t flags,
                      uint64_t ts_ms, int ts_quality, int read_type);
    void (*on_analog_output_status)(void* ctx, uint16_t index, double value, uint8_t flags,
                                    uint64_t ts_ms, int ts_quality, int read_type);
    void (*on_octet_string)(void* ctx, uint16_t index, const uint8_t* data, size_t len,
                            int read_type);

    /* Process-wide log sink. */
    void (*on_log)(void* log_ctx, int level, const char* msg);
} odc_callbacks;

/*
 * Create the DNP3 manager (asio thread pool + log handler). One per process.
 *
 *   concurrency   number of pool threads; 0 → std::thread::hardware_concurrency()
 *   cbs           callback vtable (copied)
 *   log_ctx       opaque pointer handed back to cbs.on_log
 *   log_level_mask raw opendnp3 LogLevels bitfield for channels; 0 → levels::NORMAL
 *
 * Returns NULL on allocation failure.
 */
odc_manager* odc_manager_create(uint32_t concurrency, odc_callbacks cbs, void* log_ctx,
                                int32_t log_level_mask);

/* Permanently shut down the manager and everything created under it. */
void odc_manager_destroy(odc_manager* mgr);

typedef struct {
    const char* host;
    uint16_t port;
    uint16_t master_address;
    uint16_t outstation_address;

    uint32_t response_timeout_ms; /* 0 → 5000 */
    uint32_t keep_alive_ms;       /* link keep-alive; 0 → 60000 */

    int disable_unsol_on_startup; /* bool */

    /* Unsolicited event-class mask (which of class 1/2/3 to enable for unsol). */
    int unsol_class1;
    int unsol_class2;
    int unsol_class3;

    /* Startup integrity scan class mask. All-zero → no startup integrity scan. */
    int startup_integrity_class0;
    int startup_integrity_class1;
    int startup_integrity_class2;
    int startup_integrity_class3;
} odc_master_config;

/*
 * Add a persistent TCP-client channel and bind a single master to it.
 * `ctx` is passed back on every per-association callback. Returns NULL on failure.
 */
odc_master* odc_manager_add_master(odc_manager* mgr, const char* id,
                                   odc_master_config cfg, void* ctx);

/* Register a recurring class-based scan. Returns 0 on success, non-zero on error. */
int odc_master_add_class_scan(odc_master* mst, int class0, int class1, int class2,
                              int class3, uint32_t period_ms);

/* Register a recurring all-objects (qualifier 0x06) scan for a group/variation. */
int odc_master_add_all_objects_scan(odc_master* mst, uint8_t group, uint8_t variation,
                                    uint32_t period_ms);

/* Issue a one-shot class scan (e.g. an integrity poll). Returns 0 on success. */
int odc_master_scan_classes(odc_master* mst, int class0, int class1, int class2, int class3);

/* Synchronously enable/disable master communications. Returns 0 on success. */
int odc_master_enable(odc_master* mst);
int odc_master_disable(odc_master* mst);

/* Shut down and free a single master and its channel. */
void odc_master_destroy(odc_master* mst);

/* ===========================================================================
 * Outstation server (gateway acts as a DNP3 outstation, serving aggregated
 * field data northbound to a SCADA master).
 *
 * Hangs off the SAME odc_manager as the masters (one asio thread pool, one log
 * sink for both roles). Monitoring-only for now: the command handler rejects
 * every control with NOT_SUPPORTED; SCADA→field passthrough comes later.
 * ===========================================================================*/

typedef struct odc_outstation odc_outstation;

/*
 * Per-type point counts → DatabaseConfig sizing. The outstation database is
 * built with contiguous indices [0, count) for each type, default class 1 and
 * default static/event variations. Updates to an out-of-range index are
 * dropped, so size each type to cover the highest index the gateway serves.
 */
typedef struct {
    uint16_t binary;
    uint16_t double_bit;
    uint16_t analog;
    uint16_t counter;
    uint16_t frozen_counter; /* sized for completeness; no direct setter — opendnp3
                                derives frozen counters by freezing a counter, so
                                map a field frozen-counter to a plain counter point */
    uint16_t binary_output_status;
    uint16_t analog_output_status;
    uint16_t octet_string;
} odc_db_sizes;

/*
 * Outstation callback vtable. Currently just the channel-state callback, which
 * for a TCP server reflects whether a master (SCADA) is connected. `ctx` is the
 * opaque pointer passed to odc_manager_add_outstation. May be NULL.
 */
typedef struct {
    void (*on_channel_state)(void* ctx, int state);
} odc_outstation_callbacks;

typedef struct {
    const char* bind_host;       /* listen address; NULL → "0.0.0.0" */
    uint16_t    port;            /* DNP3/IP listen port (default 20000) */
    uint16_t    local_address;   /* this outstation's link-layer address */
    uint16_t    master_address;  /* the SCADA master's link-layer address */

    int         allow_unsolicited;   /* permit unsolicited responses */
    uint16_t    event_buffer_size;   /* per-type event buffer depth; 0 → 100 */

    odc_db_sizes sizes;
} odc_outstation_config;

/*
 * Add a TCP-server channel to the manager and bind one outstation to it.
 * `ctx` is passed back on the channel-state callback. Returns NULL on failure.
 */
odc_outstation* odc_manager_add_outstation(odc_manager* mgr, const char* id,
                                           odc_outstation_config cfg,
                                           odc_outstation_callbacks cbs, void* ctx);

/*
 * Push one measurement into the outstation database (UpdateBuilder + Apply).
 * Apply posts to the outstation's strand, so these are safe to call from any
 * thread. flags = DNP3 quality bitfield (IEEE 1815 §A.4); ts_ms = unix epoch
 * milliseconds (0 → no timestamp / INVALID quality). Return 0 on success.
 */
int odc_outstation_update_binary(odc_outstation* os, uint16_t index, int value, uint8_t flags, uint64_t ts_ms);
int odc_outstation_update_double_bit(odc_outstation* os, uint16_t index, int state, uint8_t flags, uint64_t ts_ms);
int odc_outstation_update_analog(odc_outstation* os, uint16_t index, double value, uint8_t flags, uint64_t ts_ms);
int odc_outstation_update_counter(odc_outstation* os, uint16_t index, uint32_t value, uint8_t flags, uint64_t ts_ms);
int odc_outstation_update_binary_output_status(odc_outstation* os, uint16_t index, int value, uint8_t flags, uint64_t ts_ms);
int odc_outstation_update_analog_output_status(odc_outstation* os, uint16_t index, double value, uint8_t flags, uint64_t ts_ms);
int odc_outstation_update_octet_string(odc_outstation* os, uint16_t index, const uint8_t* data, size_t len);

/* Synchronously enable/disable the outstation (start/stop serving). 0 on success. */
int  odc_outstation_enable(odc_outstation* os);
int  odc_outstation_disable(odc_outstation* os);

/* Shut down and free a single outstation and its server channel. */
void odc_outstation_destroy(odc_outstation* os);

#ifdef __cplusplus
}
#endif

#endif /* OPENDNP3_C_H */
