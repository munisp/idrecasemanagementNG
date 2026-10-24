FROM golang:1.23-bookworm AS build
LABEL org.opencontainers.image.authors="Patrick Munis at newwave"
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
# tigerbeetle-go wraps a native C client library via cgo -- its package build
# constraints exclude all Go files when CGO_ENABLED=0, so this must build with
# cgo on (and the final stage needs a libc, not the fully static base).
RUN CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/case-api ./cmd/server

FROM gcr.io/distroless/base-debian12
COPY --from=build /out/case-api /case-api
EXPOSE 8080
USER nonroot
ENTRYPOINT ["/case-api"]
