//go:build dnp3_ffi

// Command godnp3smoke is the library's runtime smoke test: it runs a goDnp3
// Outstation server and a goDnp3 Master in one process over loopback, pushes a
// known set of point values into the outstation, then verifies the master reads
// them back — values + quality, including a fractional analog (proving analogs
// are served as float, not the opendnp3 integer default) and a bad-quality
// point. Also exercises the shared refcounted DNP3Manager (both roles at once).
//
// Build:  go build -tags dnp3_ffi ./cmd/godnp3smoke   (with the opendnp3 cgo flags)
// Run:    godnp3smoke -port 20123 -duration 8s
//
// Exit 0 only if the master connected AND read back every served point with the
// expected value and quality; non-zero otherwise.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"sync"
	"time"

	godnp3 "goDnp3"
)

// served is one point the outstation exposes, with the value the master should
// read back.
type served struct {
	pt  godnp3.PointType
	idx uint16
	q   godnp3.Quality
	b   bool
	f   float64
	u   uint32
	dbb godnp3.DoubleBitState
}

func (sv served) meas() godnp3.Measurement {
	m := godnp3.Measurement{PointType: sv.pt, Index: sv.idx, Time: time.Now(), Quality: sv.q}
	switch sv.pt {
	case godnp3.PointBinary, godnp3.PointBinaryOutputStatus:
		m.BoolValue = sv.b
	case godnp3.PointDoubleBitBinary:
		m.DBBValue = sv.dbb
	case godnp3.PointCounter, godnp3.PointFrozenCounter:
		m.UintValue = sv.u
	case godnp3.PointAnalog, godnp3.PointAnalogOutputStatus:
		m.FloatValue = sv.f
	}
	return m
}

const good = godnp3.QualityOnline

// Final value-set the master must read back. Fractional analogs prove the float
// wire variation; binary idx 1 is served bad (quality 0) to prove bad quality.
var expected = []served{
	{pt: godnp3.PointBinary, idx: 0, q: good, b: true},
	{pt: godnp3.PointBinary, idx: 1, q: 0, b: false},
	{pt: godnp3.PointAnalog, idx: 0, q: good, f: 123.5},
	{pt: godnp3.PointAnalog, idx: 1, q: good, f: -7.25},
	{pt: godnp3.PointCounter, idx: 0, q: good, u: 4242},
	{pt: godnp3.PointDoubleBitBinary, idx: 0, q: good, dbb: godnp3.DBBOn},
}

// Pushed before the master connects (different values) so the post-connect
// re-push + integrity poll proves the live Update path.
var initial = []served{
	{pt: godnp3.PointBinary, idx: 0, q: good, b: false},
	{pt: godnp3.PointBinary, idx: 1, q: good, b: true},
	{pt: godnp3.PointAnalog, idx: 0, q: good, f: 1},
	{pt: godnp3.PointAnalog, idx: 1, q: good, f: 2},
	{pt: godnp3.PointCounter, idx: 0, q: good, u: 1},
	{pt: godnp3.PointDoubleBitBinary, idx: 0, q: good, dbb: godnp3.DBBOff},
}

type key struct {
	pt  godnp3.PointType
	idx uint16
}

// recorder is the master-side Handler; keeps the latest measurement per point.
type recorder struct {
	mu        sync.Mutex
	latest    map[key]godnp3.Measurement
	connected bool
}

func newRecorder() *recorder { return &recorder{latest: make(map[key]godnp3.Measurement)} }

func (r *recorder) OnMeasurement(m godnp3.Measurement) {
	r.mu.Lock()
	r.latest[key{m.PointType, m.Index}] = m
	r.mu.Unlock()
}

func (r *recorder) OnStatusChange(s godnp3.Status) {
	r.mu.Lock()
	if s.Connected {
		r.connected = true
	}
	r.mu.Unlock()
	log.Printf("[master] connected=%v err=%q rx=%d", s.Connected, s.LastError, s.MeasurementsRx)
}

func (r *recorder) OnLog(level, msg string) { log.Printf("[master:%s] %s", level, msg) }

// osHandler is the outstation-side Handler (status/log only).
type osHandler struct{}

func (osHandler) OnMeasurement(godnp3.Measurement) {}
func (osHandler) OnStatusChange(s godnp3.Status) {
	log.Printf("[ostn] connected=%v err=%q served=%d", s.Connected, s.LastError, s.MeasurementsRx)
}
func (osHandler) OnLog(level, msg string) { log.Printf("[ostn:%s] %s", level, msg) }

func main() {
	port := flag.Int("port", 20123, "loopback TCP port")
	duration := flag.Duration("duration", 8*time.Second, "total test duration")
	flag.Parse()

	sizes := godnp3.DBSizes{Binary: 2, Analog: 2, Counter: 1, DoubleBit: 1}
	ostn := godnp3.NewOutstation(godnp3.ServerConfig{
		ID: "gw-ostn", Label: "Gateway Outstation", BindHost: "127.0.0.1", Port: *port,
		LocalAddress: 1024, MasterAddress: 1, EventBufferSize: 100,
	}, sizes, osHandler{})
	if err := ostn.Start(context.Background()); err != nil {
		log.Fatalf("outstation Start: %v", err)
	}
	defer ostn.Stop()
	log.Printf("outstation serving on 127.0.0.1:%d (addr 1024, master 1)", *port)

	for _, sv := range initial {
		ostn.Update(sv.meas())
	}

	rec := newRecorder()
	m := godnp3.NewMaster(rec)
	if err := m.AddOutstation(godnp3.OutstationConfig{
		ID: "gw", Label: "Gateway (loopback)", Host: "127.0.0.1", Port: *port,
		MasterAddress: 1, OutstationAddress: 1024,
		ResponseTimeoutMs: 3000, KeepAliveMs: 60000, Class1ScanMs: 1000,
		StartupIntegrity: true,
	}); err != nil {
		log.Fatalf("AddOutstation: %v", err)
	}
	if err := m.Start(context.Background()); err != nil {
		log.Fatalf("master Start: %v", err)
	}

	half := *duration / 2
	time.Sleep(half)
	log.Printf("pushing final values + on-demand integrity poll")
	for _, sv := range expected {
		ostn.Update(sv.meas())
	}
	if err := m.IntegrityPoll("gw"); err != nil {
		log.Printf("IntegrityPoll error: %v", err)
	}
	time.Sleep(*duration - half)
	m.Stop()

	rec.mu.Lock()
	defer rec.mu.Unlock()

	fmt.Println("\n──────── goDnp3 smoke summary ────────")
	fmt.Printf("master connected: %v\n", rec.connected)
	fails := 0
	for _, sv := range expected {
		got, ok := rec.latest[key{sv.pt, sv.idx}]
		if !ok {
			fmt.Printf("  MISS  %-20s idx=%d\n", sv.pt, sv.idx)
			fails++
			continue
		}
		ok = valueMatches(sv, got) && got.Quality.Good() == (sv.q == good)
		status := "ok"
		if !ok {
			status = "FAIL"
			fails++
		}
		fmt.Printf("  %-4s  %-20s idx=%d want(%s) got(%s) q=0x%02x good=%v\n",
			status, sv.pt, sv.idx, wantStr(sv), gotStr(got), uint8(got.Quality), got.Quality.Good())
	}
	fmt.Println("──────────────────────────────────────")

	if !rec.connected {
		fmt.Println("FAIL: master never connected")
		os.Exit(1)
	}
	if fails > 0 {
		fmt.Printf("FAIL: %d point(s) mismatched\n", fails)
		os.Exit(1)
	}
	fmt.Println("PASS")
}

func valueMatches(sv served, got godnp3.Measurement) bool {
	switch sv.pt {
	case godnp3.PointBinary, godnp3.PointBinaryOutputStatus:
		return got.BoolValue == sv.b
	case godnp3.PointDoubleBitBinary:
		return got.DBBValue == sv.dbb
	case godnp3.PointCounter, godnp3.PointFrozenCounter:
		return got.UintValue == sv.u
	case godnp3.PointAnalog, godnp3.PointAnalogOutputStatus:
		return math.Abs(got.FloatValue-sv.f) < 1e-9
	}
	return false
}

func wantStr(sv served) string {
	switch sv.pt {
	case godnp3.PointBinary, godnp3.PointBinaryOutputStatus:
		return fmt.Sprintf("%v", sv.b)
	case godnp3.PointDoubleBitBinary:
		return fmt.Sprintf("%d", sv.dbb)
	case godnp3.PointCounter, godnp3.PointFrozenCounter:
		return fmt.Sprintf("%d", sv.u)
	case godnp3.PointAnalog, godnp3.PointAnalogOutputStatus:
		return fmt.Sprintf("%g", sv.f)
	}
	return "?"
}

func gotStr(m godnp3.Measurement) string {
	switch m.PointType {
	case godnp3.PointBinary, godnp3.PointBinaryOutputStatus:
		return fmt.Sprintf("%v", m.BoolValue)
	case godnp3.PointDoubleBitBinary:
		return fmt.Sprintf("%d", m.DBBValue)
	case godnp3.PointCounter, godnp3.PointFrozenCounter:
		return fmt.Sprintf("%d", m.UintValue)
	case godnp3.PointAnalog, godnp3.PointAnalogOutputStatus:
		return fmt.Sprintf("%g", m.FloatValue)
	}
	return "?"
}
