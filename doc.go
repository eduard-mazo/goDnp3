// Package godnp3 is a reusable DNP3 master + outstation binding over opendnp3
// (Apache 2.0), shared by goMqttModbus (edge) and go104 (SCADA-side master).
//
// The opendnp3 C++ stack is wrapped by the flat C shim in opendnp3_c.{h,cpp}.
// The Go binding (Master, Outstation, one shared refcounted DNP3Manager) lands
// under the `dnp3_ffi` build tag; pure-Go stubs build by default so consumers
// compile without the native lib. opendnp3 is vendored under
// third_party/opendnp3/<triple>/ via `make opendnp3-vendor` (see the Makefile).
//
// Status: extraction in progress (Part 1). A1 = shim + vendoring (done);
// A2 = neutral types; A3 = the Go binding.
package godnp3
