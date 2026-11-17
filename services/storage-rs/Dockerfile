# Author: Patrick Munis at newwave
FROM rust:1.90-bookworm AS build
LABEL org.opencontainers.image.authors="Patrick Munis at newwave"
WORKDIR /src
COPY Cargo.toml Cargo.lock* ./
RUN mkdir src && echo 'fn main(){}' > src/main.rs && mkdir -p .cargo && cargo build --release && rm -rf src
COPY src ./src
RUN touch src/main.rs && cargo build --release

FROM gcr.io/distroless/cc-debian12
COPY --from=build /src/target/release/storage /storage
EXPOSE 8094
USER nonroot
ENTRYPOINT ["/storage"]
