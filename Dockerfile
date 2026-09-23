# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27.1-bookworm@sha256:69a7b9788769bec032d238959b61854e9ae87f57be9029ec04e9885fabf99195 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -buildvcs=false -o /out/oci-accelerator ./cmd/oci-accelerator
RUN mkdir -p /runtime/var/lib/oci-accelerator \
    && chown 10001:10001 /runtime/var/lib/oci-accelerator \
    && chmod 0700 /runtime/var/lib/oci-accelerator

FROM alpine:3.22@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8
COPY --from=build /runtime/ /
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/oci-accelerator /usr/local/bin/oci-accelerator
USER 10001:10001
EXPOSE 3128
STOPSIGNAL SIGTERM
ENTRYPOINT ["oci-accelerator"]
