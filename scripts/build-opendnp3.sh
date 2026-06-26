#!/usr/bin/env bash
#
# Fetch + build opendnp3 (Apache 2.0) and vendor it under third_party/opendnp3/.
# Run once per build host; output is consumed by `make build-ffi` / `icr323x-ffi`
# in the dnp3-oss branch.
#
# Usage:
#   scripts/build-opendnp3.sh                 # host triple (x86_64 glibc)
#   scripts/build-opendnp3.sh armv7-linux     # cross-compile for ICR-3232
#   scripts/build-opendnp3.sh ppc64le-linux   # cross-compile for IBM POWER (ppc64le)
#   scripts/build-opendnp3.sh windows-mingw   # cross-compile for Windows x64
#
# Prereqs (host build):
#   apt install cmake build-essential libssl-dev
# Prereqs (ARMv7 cross):
#   apt install cmake gcc-arm-linux-gnueabihf g++-arm-linux-gnueabihf
#   plus an OpenSSL static build for armv7 (script will warn if missing).
# Prereqs (Windows cross):
#   apt install cmake g++-mingw-w64-x86-64
#   (TLS is disabled — no MinGW OpenSSL needed; the sim uses plain TCP.)

set -euo pipefail

OPENDNP3_VERSION="${OPENDNP3_VERSION:-3.1.2}"
OPENDNP3_TAR_URL="https://github.com/dnp3/opendnp3/archive/refs/tags/${OPENDNP3_VERSION}.tar.gz"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="${REPO_ROOT}/build/opendnp3"
TARGET="${1:-host}"

case "${TARGET}" in
  host)
    TRIPLE="$(gcc -dumpmachine)"          # e.g. x86_64-linux-gnu
    # Normalize to the layout we use under third_party/opendnp3/.
    case "${TRIPLE}" in
      x86_64-linux-gnu) TRIPLE="x86_64-unknown-linux-gnu" ;;
      aarch64-linux-gnu) TRIPLE="aarch64-unknown-linux-gnu" ;;
    esac
    TOOLCHAIN_ARGS=()
    ;;
  armv7-linux)
    TRIPLE="armv7-unknown-linux-gnueabihf"
    if ! command -v arm-linux-gnueabihf-g++ >/dev/null; then
      echo "ERROR: arm-linux-gnueabihf-g++ not found." >&2
      echo "  apt install gcc-arm-linux-gnueabihf g++-arm-linux-gnueabihf" >&2
      exit 1
    fi
    TOOLCHAIN_FILE="${WORK}/toolchain-armv7.cmake"
    mkdir -p "${WORK}"
    cat > "${TOOLCHAIN_FILE}" <<EOF
set(CMAKE_SYSTEM_NAME Linux)
set(CMAKE_SYSTEM_PROCESSOR arm)
set(CMAKE_C_COMPILER arm-linux-gnueabihf-gcc)
set(CMAKE_CXX_COMPILER arm-linux-gnueabihf-g++)
set(CMAKE_FIND_ROOT_PATH_MODE_PROGRAM NEVER)
set(CMAKE_FIND_ROOT_PATH_MODE_LIBRARY ONLY)
set(CMAKE_FIND_ROOT_PATH_MODE_INCLUDE ONLY)
EOF
    TOOLCHAIN_ARGS=("-DCMAKE_TOOLCHAIN_FILE=${TOOLCHAIN_FILE}")
    ;;
  ppc64le-linux)
    TRIPLE="powerpc64le-unknown-linux-gnu"
    if ! command -v powerpc64le-linux-gnu-g++ >/dev/null; then
      echo "ERROR: powerpc64le-linux-gnu-g++ not found." >&2
      echo "  apt install gcc-powerpc64le-linux-gnu g++-powerpc64le-linux-gnu" >&2
      exit 1
    fi
    TOOLCHAIN_FILE="${WORK}/toolchain-ppc64le.cmake"
    mkdir -p "${WORK}"
    cat > "${TOOLCHAIN_FILE}" <<EOF
set(CMAKE_SYSTEM_NAME Linux)
set(CMAKE_SYSTEM_PROCESSOR ppc64le)
set(CMAKE_C_COMPILER powerpc64le-linux-gnu-gcc)
set(CMAKE_CXX_COMPILER powerpc64le-linux-gnu-g++)
set(CMAKE_FIND_ROOT_PATH_MODE_PROGRAM NEVER)
set(CMAKE_FIND_ROOT_PATH_MODE_LIBRARY ONLY)
set(CMAKE_FIND_ROOT_PATH_MODE_INCLUDE ONLY)
EOF
    TOOLCHAIN_ARGS=("-DCMAKE_TOOLCHAIN_FILE=${TOOLCHAIN_FILE}")
    ;;
  windows-mingw)
    TRIPLE="x86_64-w64-mingw32"
    if ! command -v x86_64-w64-mingw32-g++ >/dev/null; then
      echo "ERROR: x86_64-w64-mingw32-g++ not found." >&2
      echo "  apt install g++-mingw-w64-x86-64" >&2
      exit 1
    fi
    TOOLCHAIN_FILE="${WORK}/toolchain-mingw.cmake"
    mkdir -p "${WORK}"
    # ASIO (bundled in opendnp3) needs _WIN32_WINNT set, else it errors on the
    # target Windows version. 0x0601 = Windows 7, opendnp3's documented floor.
    cat > "${TOOLCHAIN_FILE}" <<EOF
set(CMAKE_SYSTEM_NAME Windows)
set(CMAKE_SYSTEM_PROCESSOR x86_64)
set(CMAKE_C_COMPILER x86_64-w64-mingw32-gcc)
set(CMAKE_CXX_COMPILER x86_64-w64-mingw32-g++)
set(CMAKE_RC_COMPILER x86_64-w64-mingw32-windres)
set(CMAKE_C_FLAGS_INIT "-D_WIN32_WINNT=0x0601")
set(CMAKE_CXX_FLAGS_INIT "-D_WIN32_WINNT=0x0601")
set(CMAKE_FIND_ROOT_PATH /usr/x86_64-w64-mingw32)
set(CMAKE_FIND_ROOT_PATH_MODE_PROGRAM NEVER)
set(CMAKE_FIND_ROOT_PATH_MODE_LIBRARY ONLY)
set(CMAKE_FIND_ROOT_PATH_MODE_INCLUDE ONLY)
EOF
    TOOLCHAIN_ARGS=("-DCMAKE_TOOLCHAIN_FILE=${TOOLCHAIN_FILE}")
    ;;
  *)
    echo "ERROR: unknown target '${TARGET}' (use host | armv7-linux | ppc64le-linux | windows-mingw)" >&2
    exit 1
    ;;
esac

INSTALL_DIR="${REPO_ROOT}/third_party/opendnp3/${TRIPLE}"
BUILD_DIR="${WORK}/build-${TRIPLE}"
SRC_DIR="${WORK}/src-${OPENDNP3_VERSION}"

echo "==> opendnp3 ${OPENDNP3_VERSION}"
echo "    target triple : ${TRIPLE}"
echo "    install dir   : ${INSTALL_DIR}"
echo "    build dir     : ${BUILD_DIR}"
echo

# --- 1. Preflight ---------------------------------------------------------
for tool in cmake make tar curl; do
  if ! command -v "${tool}" >/dev/null; then
    echo "ERROR: '${tool}' not found in PATH." >&2
    case "${tool}" in
      cmake) echo "  apt install cmake" >&2 ;;
      make)  echo "  apt install build-essential" >&2 ;;
    esac
    exit 1
  fi
done

# OpenSSL headers are needed for TLS support. We don't hard-fail if absent,
# but warn and disable TLS so the build still produces a usable lib.
DNP3_TLS=ON
if [ "${TARGET}" = "host" ]; then
  if ! [ -f /usr/include/openssl/ssl.h ]; then
    echo "WARN: /usr/include/openssl/ssl.h not found — building without TLS." >&2
    echo "      apt install libssl-dev   (to enable g120/g121 secure DNP3 later)" >&2
    DNP3_TLS=OFF
  fi
else
  # Cross-compile: OpenSSL for ARMv7 is rarely present by default. Disable
  # TLS unless OPENSSL_ROOT_DIR points at a prebuilt static install.
  if [ -z "${OPENSSL_ROOT_DIR:-}" ]; then
    echo "WARN: OPENSSL_ROOT_DIR not set for cross build — disabling DNP3 TLS." >&2
    DNP3_TLS=OFF
  fi
fi

# --- 2. Fetch source ------------------------------------------------------
mkdir -p "${WORK}"
if [ ! -d "${SRC_DIR}" ]; then
  echo "==> fetching opendnp3 ${OPENDNP3_VERSION}"
  curl -sSL -o "${WORK}/opendnp3-${OPENDNP3_VERSION}.tar.gz" "${OPENDNP3_TAR_URL}"
  tar xzf "${WORK}/opendnp3-${OPENDNP3_VERSION}.tar.gz" -C "${WORK}"
  mv "${WORK}/opendnp3-${OPENDNP3_VERSION}" "${SRC_DIR}"
fi

# --- 3. Configure ---------------------------------------------------------
rm -rf "${BUILD_DIR}"
mkdir -p "${BUILD_DIR}"

cmake -S "${SRC_DIR}" -B "${BUILD_DIR}" \
  -DCMAKE_BUILD_TYPE=Release \
  -DCMAKE_INSTALL_PREFIX="${INSTALL_DIR}" \
  -DCMAKE_POSITION_INDEPENDENT_CODE=ON \
  -DDNP3_STATIC_LIBS=ON \
  -DDNP3_TLS=${DNP3_TLS} \
  -DDNP3_EXAMPLES=OFF \
  -DDNP3_TESTS=OFF \
  "${TOOLCHAIN_ARGS[@]}"

# --- 4. Build + install ---------------------------------------------------
NPROC="$(nproc 2>/dev/null || echo 2)"
echo "==> building with ${NPROC} jobs (this takes ~5 min on a laptop)"
cmake --build "${BUILD_DIR}" --target install -j "${NPROC}"

# --- 5. Sanity check ------------------------------------------------------
echo
echo "==> install contents:"
find "${INSTALL_DIR}" -maxdepth 3 -type f \( -name '*.h' -o -name '*.hpp' -o -name '*.a' \) | sort | head -20
echo
echo "==> static libs:"
ls -lh "${INSTALL_DIR}/lib/"*.a 2>/dev/null || echo "  (no .a files — install layout differs, inspect ${INSTALL_DIR}/)"
echo
echo "OK: opendnp3 ${OPENDNP3_VERSION} vendored at ${INSTALL_DIR}"
