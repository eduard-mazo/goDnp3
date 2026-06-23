//go:build !dnp3_ffi

package godnp3

import (
	"context"
	"fmt"
	"sync"
)

// stubMaster is the default no-op implementation. It tracks registered
// outstations and reports them as disconnected without performing any I/O.
// Lets the rest of the gateway (UI, API, MQTT, Sparkplug) be built, run, and
// exercised end-to-end before the CGO binding is in place.
type stubMaster struct {
	h  Handler
	mu sync.Mutex
	os map[string]OutstationConfig
}

func newMaster(h Handler) Master {
	return &stubMaster{
		h:  h,
		os: make(map[string]OutstationConfig),
	}
}

func (m *stubMaster) AddOutstation(o OutstationConfig) error {
	m.mu.Lock()
	m.os[o.ID] = o
	m.mu.Unlock()
	return nil
}

func (m *stubMaster) RemoveOutstation(id string) error {
	m.mu.Lock()
	delete(m.os, id)
	m.mu.Unlock()
	return nil
}

func (m *stubMaster) Start(_ context.Context) error {
	if m.h != nil {
		m.h.OnLog("warn", "DNP3 master running in STUB mode — no measurements will be received. Build with -tags dnp3_ffi after running `make opendnp3-vendor`.")
	}
	return nil
}

func (m *stubMaster) Stop() {}

func (m *stubMaster) Status() []Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Status, 0, len(m.os))
	for _, o := range m.os {
		out = append(out, Status{
			ID:        o.ID,
			Label:     o.Label,
			Addr:      o.Addr(),
			Connected: false,
			LastError: "stub build (no DNP3 library)",
		})
	}
	return out
}

func (m *stubMaster) IntegrityPoll(id string) error {
	return fmt.Errorf("stub master: integrity poll not implemented (build with -tags dnp3_ffi)")
}
