package godnp3

import (
	"strconv"
	"time"
)

// PointType is the DNP3 object class of a measurement. Values match IEEE 1815
// object groups (e.g. binary = g1/g2, analog = g30/g32). The string values are
// kept identical to goMqttModbus's source.PointType so an adapter can cast.
type PointType string

const (
	PointBinary             PointType = "binary"
	PointDoubleBitBinary    PointType = "double_bit_binary"
	PointBinaryOutputStatus PointType = "binary_output_status"
	PointCounter            PointType = "counter"
	PointFrozenCounter      PointType = "frozen_counter"
	PointAnalog             PointType = "analog"
	PointAnalogOutputStatus PointType = "analog_output_status"
	PointOctetString        PointType = "octet_string"
)

// Quality is the DNP3 flag bitfield (IEEE 1815 §A.4) carried with every
// measurement. The bit layout matches opendnp3's Flags byte for each type, so
// it crosses the C shim unchanged.
type Quality uint8

const (
	QualityOnline       Quality = 1 << 0
	QualityRestart      Quality = 1 << 1
	QualityCommLost     Quality = 1 << 2
	QualityRemoteForced Quality = 1 << 3
	QualityLocalForced  Quality = 1 << 4
	QualityOverRange    Quality = 1 << 5 // analog only
	QualityRefError     Quality = 1 << 6 // analog only
	QualityChatter      Quality = 1 << 5 // binary only (alias of OverRange bit)
)

// Good reports whether the flags indicate a usable measurement
// (online and not restart/comm-lost).
func (q Quality) Good() bool {
	return q&QualityOnline != 0 &&
		q&QualityRestart == 0 &&
		q&QualityCommLost == 0
}

// DoubleBitState encodes the DNP3 g3/g4 double-bit binary state.
type DoubleBitState uint8

const (
	DBBIntermediate DoubleBitState = 0
	DBBOff          DoubleBitState = 1
	DBBOn           DoubleBitState = 2
	DBBIndeterm     DoubleBitState = 3
)

// Measurement is one point value crossing the binding boundary in either
// direction: delivered by a Master to its Handler (OnMeasurement), or pushed
// into an Outstation server (Update). Exactly one value field is meaningful,
// per PointType.
type Measurement struct {
	OutstationID string // which outstation it came from (master); ignored by Update
	PointType    PointType
	Index        uint16
	Time         time.Time // value timestamp from the device (or time.Now if absent)
	Quality      Quality

	// IsEvent is true when the value arrived as a spontaneous event rather than
	// a poll/static response (master side only).
	IsEvent bool

	BoolValue  bool
	DBBValue   DoubleBitState // double-bit binary
	UintValue  uint32         // counter / frozen counter
	FloatValue float64        // analog / analog output status
	BytesValue []byte         // octet string
}

// Status is a per-outstation (master) or per-server (outstation) snapshot.
type Status struct {
	ID              string    `json:"id"`
	Label           string    `json:"label"`
	Addr            string    `json:"addr"`
	Connected       bool      `json:"connected"`
	LastError       string    `json:"lastError"`
	MeasurementsRx  int64     `json:"measurementsRx"`
	IntegrityPolls  int64     `json:"integrityPolls"`
	ClassPolls      int64     `json:"classPolls"`
	UnsolicitedRsps int64     `json:"unsolicitedRsps"`
	LastReadAt      time.Time `json:"lastReadAt"`
}

// Handler receives measurements, status changes, and log lines from a Master or
// Outstation. Implementations must be safe for concurrent calls from multiple
// opendnp3 strand threads.
type Handler interface {
	OnMeasurement(m Measurement)
	OnStatusChange(s Status)
	OnLog(level, msg string)
}

// OutstationConfig describes a remote DNP3 outstation a Master connects to and
// polls (one TCP channel per host:port; one association per link-address pair).
// Caller-side concerns like an "enabled" gate are out of scope — only enabled
// outstations are added.
type OutstationConfig struct {
	ID    string // unique key (used for status + scan addressing)
	Label string // human-readable name (display only)
	Host  string
	Port  int // DNP3/IP default 20000

	MasterAddress     uint16 // local link-layer address (typical 1)
	OutstationAddress uint16 // remote link-layer address (typical 1024+)
	ResponseTimeoutMs int    // app-layer response timeout; default 5000
	KeepAliveMs       int    // app-layer keep-alive interval; default 60000

	// Polling cadence (0 = disabled).
	IntegrityScanMs int // periodic integrity poll (class 0+1+2+3)
	Class1ScanMs    int // event class 1 poll
	Class2ScanMs    int // event class 2 poll
	Class3ScanMs    int // event class 3 poll

	// Unsolicited responses.
	UnsolicitedEnabled bool
	UnsolicitedClass1  bool
	UnsolicitedClass2  bool
	UnsolicitedClass3  bool

	// Startup behavior.
	DisableUnsolOnStartup bool // DISABLE_UNSOLICITED before the initial integrity poll
	StartupIntegrity      bool // perform an integrity poll on connect

	// Startup-integrity class selection. If StartupIntegrity is set but no class
	// flag is, the lib treats it as "all classes" (DNP3 conformant default).
	IntegrityClass0 bool
	IntegrityClass1 bool
	IntegrityClass2 bool
	IntegrityClass3 bool

	// StaticPollMs enables periodic group-specific static reads ("all objects"
	// qualifier) at this period in ms (0 = disabled) — for outstations that don't
	// flag events on update.
	StaticPollMs int
}

// Addr returns "host:port", defaulting the port to the DNP3/IP port 20000.
func (o OutstationConfig) Addr() string {
	port := o.Port
	if port == 0 {
		port = 20000
	}
	return o.Host + ":" + strconv.Itoa(port)
}

// ServerConfig describes the gateway's own DNP3 outstation: a TCP server a SCADA
// master polls. Single instance per binding.
type ServerConfig struct {
	ID    string // status key; default "dnp3-server"
	Label string

	BindHost string // listen address; default 0.0.0.0
	Port     int    // DNP3/IP listen port; default 20000

	LocalAddress  uint16 // this outstation's link addr (typical 1024+)
	MasterAddress uint16 // the SCADA master's link addr (typical 1)

	AllowUnsolicited bool
	EventBufferSize  int // per-type event buffer depth; default 100
}

// ServerID returns the status key, defaulting to "dnp3-server".
func (s ServerConfig) ServerID() string {
	if s.ID != "" {
		return s.ID
	}
	return "dnp3-server"
}

// Bind returns the listen address, defaulting the host to 0.0.0.0.
func (s ServerConfig) Bind() string {
	if s.BindHost != "" {
		return s.BindHost
	}
	return "0.0.0.0"
}

// ServerPort returns the listen port, defaulting to 20000.
func (s ServerConfig) ServerPort() int {
	if s.Port == 0 {
		return 20000
	}
	return s.Port
}

// Addr returns "host:port" for the server.
func (s ServerConfig) Addr() string {
	return s.Bind() + ":" + strconv.Itoa(s.ServerPort())
}

// DBSizes is the per-type point count used to size an Outstation database. The
// database is built with contiguous indices [0, count) per type; an Update to an
// out-of-range index is dropped. FrozenCounter is sized for completeness but has
// no direct setter (opendnp3 derives frozen counters by freezing a counter).
type DBSizes struct {
	Binary             uint16
	DoubleBit          uint16
	Analog             uint16
	Counter            uint16
	FrozenCounter      uint16
	BinaryOutputStatus uint16
	AnalogOutputStatus uint16
	OctetString        uint16
}
