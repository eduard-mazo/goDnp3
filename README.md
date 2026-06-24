# goDnp3

A small, reusable **DNP3 (IEEE 1815) binding** over the Apache-2.0 [opendnp3]
C++ stack, shared by two consumers:

- **goMqttModbus** (`goEdge`) — the edge gateway. Acts as a DNP3 **master**
  (polling field outstations) and a DNP3 **outstation server** (serving its
  aggregated data northbound to SCADA).
- **go104** — the SCADA-side console. Acts as a DNP3 **master** polling the
  edge's outstation, alongside its IEC-104 lines.

[opendnp3]: https://github.com/dnp3/opendnp3

## How it's built

```
package godnp3
├── types.go            neutral vocabulary: Measurement, Quality, PointType,
│                       OutstationConfig, ServerConfig, CommandSink … (no
│                       dependency on either consumer)
├── master.go           Master / Outstation interfaces + NewMaster/NewOutstation
├── *_stub.go  (default) pure-Go no-ops — compile & run anywhere, no C needed
├── *_ffi.go   (-tags    cgo binding to the C shim; opendnp3 callbacks arrive
│              dnp3_ffi)  via //export functions
├── opendnp3_c.{h,cpp}  flat C ABI wrapping opendnp3's C++ API
└── third_party/opendnp3/<triple>/   vendored static lib + headers (gitignored)
```

Two build modes:

| Build | Command | Needs |
|---|---|---|
| **stub** (default) | `go build ./...` | nothing — DNP3 is a no-op |
| **real** | `go build -tags dnp3_ffi …` | opendnp3 vendored + cgo flags |

Vendor opendnp3 once per build host (needs `cmake`, `g++`, `libssl-dev`):

```sh
make opendnp3-vendor          # host
make opendnp3-vendor-arm      # ICR-3232 arm/v7
make smoke                    # loopback master↔outstation self-test (PASS)
```

## How consumers depend on it — sibling checkout

goMqttModbus and go104 reference this module by a **filesystem path**, not a
remote version:

```
// in goMqttModbus/go.mod and go104/go.mod
require goDnp3 v0.0.0
replace goDnp3 => ../goDnp3
```

So the three repos must be **siblings under the same parent directory**:

```
<workspace>/
├── goDnp3/         (this repo)
├── goMqttModbus/   replace ../goDnp3
└── go104/          replace ../goDnp3
```

This keeps cross-repo edits instant (no re-tagging) and builds offline.

## Replicating on another device

```sh
cd <workspace>
git clone git@github.com:eduard-mazo/goDnp3.git       goDnp3
git clone git@github.com:eduard-mazo/goEdge.git       goMqttModbus
git clone git@github.com:eduard-mazo/go104.git        go104

# pure-Go builds work immediately (DNP3 = stub):
( cd goMqttModbus && go build ./... )
( cd go104        && go build ./... )

# for real DNP3 (cgo), vendor opendnp3 once, then build with the ffi target:
( cd goDnp3 && make opendnp3-vendor )
( cd goMqttModbus && make build-ffi-noembed )
( cd go104 && make backend-ffi )
```

Keep the three repos side-by-side and the `replace` resolves automatically.
