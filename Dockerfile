# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build

ARG VERSION=dev
ARG COMMIT=none
ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

# Dependencies first so they stay cached across source edits.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
        -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
        -o /out/jconfig .

# Distroless Debian 13. No git binary is needed at runtime: go-git speaks the
# protocol natively, so a static image with nothing but CA certificates and
# tzdata is enough.
FROM gcr.io/distroless/static-debian13:nonroot

LABEL org.opencontainers.image.title="jconfig" \
      org.opencontainers.image.description="Junos configuration backup into git with a Prometheus exporter" \
      org.opencontainers.image.source="https://github.com/didww/jconfig" \
      org.opencontainers.image.licenses="MIT"

COPY --from=build /out/jconfig /usr/local/bin/jconfig

# Metrics and health. The management socket stays on loopback and is reached
# with `kubectl port-forward`.
EXPOSE 9639

VOLUME ["/var/lib/jconfig"]
USER nonroot:nonroot

ENTRYPOINT ["/usr/local/bin/jconfig"]
CMD ["-config", "/etc/jconfig/jconfig.yml"]
