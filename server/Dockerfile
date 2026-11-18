# syntax=docker/dockerfile:1

# ---- build ----
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Pure-Go SQLite: no CGO needed, so the binary is fully static.
# timetzdata embeds time zones so TZ works in the minimal image.
RUN CGO_ENABLED=0 go build -trimpath -tags timetzdata -ldflags "-s -w" -o /out/droidbeam-server . \
 && mkdir -p /out/data

# ---- run ----
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/droidbeam-server /usr/local/bin/droidbeam-server
COPY --from=build --chown=nonroot:nonroot /out/data /data
ENV DROIDBEAM_ADDR=:8080 \
    DROIDBEAM_DB=/data/droidbeam.db
VOLUME ["/data"]
EXPOSE 8080
USER nonroot
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/usr/local/bin/droidbeam-server", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/droidbeam-server"]
