# syntax=docker/dockerfile:1

# Stage 1: build CoreDNS with the ztnet plugin compiled in.
FROM golang:1.26 AS builder

ARG COREDNS_VERSION

WORKDIR /build
COPY . /plugin
RUN /plugin/hack/build-coredns.sh "${COREDNS_VERSION}" /build/coredns \
    && /build/coredns -plugins | grep -w ztnet

# Stage 2: minimal runtime image.
FROM alpine:3.21
RUN apk add --no-cache ca-certificates && \
    mkdir -p /etc/coredns
COPY --from=builder /build/coredns /coredns
COPY docker/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh
EXPOSE 53 53/udp
ENTRYPOINT ["/entrypoint.sh"]
CMD ["-conf", "/etc/coredns/Corefile"]
