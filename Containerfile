FROM mirror.gcr.io/library/golang:1.26.4-alpine@sha256:3ad57304ad93bbec8548a0437ad9e06a455660655d9af011d58b993f6f615648 AS builder

ARG VERSION=development
ARG REVISION=unknown

# hadolint ignore=DL3018
RUN echo 'nobody:x:65534:65534:Nobody:/home/nobody:' > /tmp/passwd && \
    apk add --no-cache ca-certificates

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build \
    -ldflags "-s -w -X main.version=${VERSION} -X main.revision=${REVISION}" \
    -trimpath \
    -o /build/ouroboros \
    ./cmd/ouroboros

FROM scratch

COPY --from=builder /tmp/passwd /etc/passwd
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder --chmod=555 /build/ouroboros /ouroboros

USER 65534
ENTRYPOINT ["/ouroboros"]
