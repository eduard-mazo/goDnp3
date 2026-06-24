//go:build !dnp3_ffi

package godnp3

import (
	"context"
)

// stubOutstation is the default no-op implementation. It lets the gateway (UI,
// API, MQTT, config) be built and run without the opendnp3 library; no DNP3
// server is actually bound, so a SCADA master cannot connect.
type stubOutstation struct {
	cfg ServerConfig
	h   Handler
}

func newOutstation(cfg ServerConfig, _ DBSizes, h Handler) Outstation {
	return &stubOutstation{cfg: cfg, h: h}
}

func (o *stubOutstation) Start(_ context.Context) error {
	if o.h != nil {
		o.h.OnLog("warn", "DNP3 outstation server running in STUB mode — no SCADA master can connect. Build with -tags dnp3_ffi after running `make opendnp3-vendor`.")
	}
	return nil
}

func (o *stubOutstation) Stop() {}

func (o *stubOutstation) Update(Measurement) {}

func (o *stubOutstation) SetCommandSink(CommandSink) {}

func (o *stubOutstation) Status() Status {
	return Status{
		ID:        o.cfg.ServerID(),
		Label:     o.cfg.Label,
		Addr:      o.cfg.Addr(),
		Connected: false,
		LastError: "stub build (no DNP3 library)",
	}
}
