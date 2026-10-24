#!/usr/bin/env bash
# Author: Patrick Munis at newwave
# Restore Go + Rust toolchains and dependency caches from the persistent
# /mnt/agents/toolchains store — NO network, NO reinstall. Run once after any
# sandbox/server restart, then `source scripts/env.sh`.
#
# Why this exists: /mnt/agents is the only persistent storage, it rejects
# single files >~100MB, and it does not preserve exec bits — so toolchains are
# stored as tar chunks and re-extracted into $HOME (exec bits restored by tar).
set -euo pipefail

STORE=/mnt/agents/toolchains
[ -d "$STORE" ] || { echo "toolchain store missing: $STORE"; exit 1; }

echo ">> Go 1.23.4 -> \$HOME/sdk/go"
mkdir -p "$HOME/sdk"
tar -xzf "$STORE/go1.23.4.linux-amd64.tar.gz" -C "$HOME/sdk"

echo ">> Rust 1.98.1 -> \$HOME/.cargo + \$HOME/.rustup"
cat "$STORE"/rust-toolchain.tar.gz.part-* | tar -xzf - -C "$HOME"

echo ">> Go module cache -> \$HOME/go/pkg/mod"
mkdir -p "$HOME/go/pkg"
cat "$STORE"/gomodcache.tar.gz.part-* | tar -xzf - -C "$HOME/go/pkg"

echo ">> Cargo registry cache -> \$HOME/.cargo/registry"
cat "$STORE"/cargo-registry.tar.gz.part-* | tar -xzf - -C "$HOME/.cargo"

echo ">> verifying (offline)"
export PATH="$HOME/sdk/go/bin:$HOME/.cargo/bin:$PATH"
export GOPATH="$HOME/go" GOMODCACHE="$HOME/go/pkg/mod" GOCACHE="$HOME/.cache/go-build"
export CARGO_HOME="$HOME/.cargo" CARGO_TARGET_DIR="$HOME/cargo-target"
go version
rustc --version
cargo --version

echo ">> offline build smoke test"
cd "$(dirname "$0")/../services/case-api-go"
GOPROXY=off go build -buildvcs=false ./... && echo "  go build (offline): OK"
cd ../vault-rs
CARGO_NET_OFFLINE=true cargo check -q && echo "  cargo check (offline): OK"
echo "ALL TOOLCHAINS RESTORED"
