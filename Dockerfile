# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27.1-bookworm AS build
WORKDIR /src
ENV GOTOOLCHAIN=local
COPY go.mod go.sum ./
RUN go mod download
COPY VERSION version.go ./
COPY cmd/ ./cmd/
COPY internal/ ./internal/
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -buildvcs=false -o /out/oci-accelerator ./cmd/oci-accelerator

FROM alpine:3.24.1
LABEL org.opencontainers.image.source="https://github.com/buildkite/oci-accelerator"
RUN apk add --no-cache ca-certificates \
    && addgroup -S -g 10001 accelerator \
    && adduser -S -D -H -u 10001 -G accelerator accelerator \
    && mkdir -p /var/lib/oci-accelerator/cache \
    && chown -R 10001:10001 /var/lib/oci-accelerator \
    && chmod 0700 /var/lib/oci-accelerator /var/lib/oci-accelerator/cache
COPY --from=build /out/oci-accelerator /usr/local/bin/oci-accelerator
USER 10001:10001
EXPOSE 3128
STOPSIGNAL SIGTERM
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD ["wget", "-Y", "off", "-q", "-T", "2", "-O", "/dev/null", "http://127.0.0.1:9090/readyz"]
ENTRYPOINT ["/usr/local/bin/oci-accelerator"]
CMD ["--config", "/etc/oci-accelerator/config.yaml"]
