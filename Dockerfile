# One image, one binary: the whole platform.
#
# There is no SERVICE build argument any more. cmd/telemed composes every domain
# and the edge into one process, and TELEMED_DOMAINS selects a subset at RUN
# time -- so the nine-process topology is still one `docker run` away from this
# same image, without a second build.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder
RUN apk add --no-cache git ca-certificates tzdata
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY . .
ARG TARGETARCH
ARG VERSION=dev
ARG BUILD_TIME
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build \
      -trimpath \
      -ldflags="-s -w -X main.Version=${VERSION} -X main.BuildTime=${BUILD_TIME}" \
      -o /out/server ./cmd/telemed

FROM scratch
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=builder /etc/passwd /etc/passwd
COPY --from=builder /out/server /server
USER 65532:65532
ENV TZ=UTC
# 8080 is the edge. The gRPC ports are only bound when EXPOSE_GRPC is set,
# which a single-process deployment never needs.
EXPOSE 8080 9090
ENTRYPOINT ["/server"]
