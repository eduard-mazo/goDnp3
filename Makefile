# goDnp3 — reusable DNP3 (opendnp3) binding shared by goMqttModbus and go104.
#
# opendnp3 (Apache 2.0) is vendored under third_party/opendnp3/<triple>/, built
# once per host with `make opendnp3-vendor[-arm|-windows]`. The static archive
# links into each consumer's binary; nothing extra to deploy.

DNP3_HOST_TRIPLE   ?= x86_64-unknown-linux-gnu
DNP3_ARM_TRIPLE    ?= armv7-unknown-linux-gnueabihf
DNP3_WIN_TRIPLE    ?= x86_64-w64-mingw32
DNP3_HOST_DIR      := third_party/opendnp3/$(DNP3_HOST_TRIPLE)
DNP3_ARM_DIR       := third_party/opendnp3/$(DNP3_ARM_TRIPLE)
DNP3_WIN_DIR       := third_party/opendnp3/$(DNP3_WIN_TRIPLE)

# The C++ shim (opendnp3_c.cpp) is compiled by cgo in consumers; it needs the
# opendnp3 headers (CXXFLAGS) and the static libs + their TLS/stdc++/pthread
# deps (LDFLAGS). Consumers reference these same flags pointed at this module.
DNP3_HOST_CXXFLAGS := -std=c++17 -I$(CURDIR)/$(DNP3_HOST_DIR)/include
DNP3_HOST_LDFLAGS  := -L$(CURDIR)/$(DNP3_HOST_DIR)/lib -lopendnp3 -lssl -lcrypto -lstdc++ -lpthread -lm -ldl
DNP3_ARM_CXXFLAGS  := -std=c++17 -I$(CURDIR)/$(DNP3_ARM_DIR)/include
# The armv7 opendnp3 is vendored with DNP3_TLS=OFF (no armhf OpenSSL on the build
# host), so it links no ssl/crypto. Force-static the C++ runtime via the literal
# archive (-l:libstdc++.a) so the binary needs no libstdc++ on the device.
DNP3_ARM_LDFLAGS   := -L$(CURDIR)/$(DNP3_ARM_DIR)/lib -lopendnp3 -l:libstdc++.a -lpthread -lm -ldl -static-libgcc

.DEFAULT_GOAL := help
.PHONY: help opendnp3-vendor opendnp3-vendor-arm opendnp3-vendor-windows \
        check-dnp3-host check-dnp3-arm check-dnp3-windows verify-shim

help: ## Show this help
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}'

opendnp3-vendor: ## Vendor opendnp3 static libs for the host
	bash scripts/build-opendnp3.sh host

opendnp3-vendor-arm: ## Vendor opendnp3 static libs for ICR-323x arm/v7
	bash scripts/build-opendnp3.sh armv7-linux

opendnp3-vendor-windows: ## Vendor opendnp3 static libs for Windows x64
	bash scripts/build-opendnp3.sh windows-mingw

check-dnp3-host:
	@if [ ! -f $(DNP3_HOST_DIR)/include/opendnp3/DNP3Manager.h ] || [ ! -f $(DNP3_HOST_DIR)/lib/libopendnp3.a ]; then \
		echo "ERROR: missing $(DNP3_HOST_DIR)/{include/opendnp3/DNP3Manager.h,lib/libopendnp3.a}"; \
		echo "  Run: make opendnp3-vendor"; \
		exit 1; \
	fi

check-dnp3-arm:
	@if [ ! -f $(DNP3_ARM_DIR)/include/opendnp3/DNP3Manager.h ] || [ ! -f $(DNP3_ARM_DIR)/lib/libopendnp3.a ]; then \
		echo "ERROR: missing $(DNP3_ARM_DIR)/{include/opendnp3/DNP3Manager.h,lib/libopendnp3.a}"; \
		echo "  Run: make opendnp3-vendor-arm"; \
		exit 1; \
	fi

check-dnp3-windows:
	@if [ ! -f $(DNP3_WIN_DIR)/include/opendnp3/DNP3Manager.h ] || [ ! -f $(DNP3_WIN_DIR)/lib/libopendnp3.a ]; then \
		echo "ERROR: missing $(DNP3_WIN_DIR)/{include/opendnp3/DNP3Manager.h,lib/libopendnp3.a}"; \
		echo "  Run: make opendnp3-vendor-windows"; \
		exit 1; \
	fi

verify-shim: check-dnp3-host ## Standalone g++ compile-check of the C shim (no cgo)
	g++ -std=c++17 -fPIC -Wall -I$(DNP3_HOST_DIR)/include -c opendnp3_c.cpp -o /tmp/godnp3_shim.o
	@rm -f /tmp/godnp3_shim.o
	@echo "shim OK"
