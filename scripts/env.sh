# Author: Patrick Munis at newwave
# shellcheck disable=SC2148
# Source this after scripts/restore-toolchains.sh:
#   source scripts/env.sh
export PATH="$HOME/sdk/go/bin:$HOME/.cargo/bin:$PATH"

# Go
export GOPATH="$HOME/go"
export GOMODCACHE="$HOME/go/pkg/mod"
export GOCACHE="$HOME/.cache/go-build"
# Mirrors (only needed if you ADD new dependencies; existing builds are offline)
export GOPROXY="https://goproxy.cn,direct"
export GOSUMDB="sum.golang.google.cn"

# Rust
export CARGO_HOME="$HOME/.cargo"
export CARGO_TARGET_DIR="$HOME/cargo-target"
export RUSTUP_HOME="$HOME/.rustup"
# crates mirror for NEW deps: rsproxy sparse index is configured in
# services/vault-rs/.cargo/config.toml
