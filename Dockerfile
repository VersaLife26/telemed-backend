# One image per entrypoint, selected at build time with --build-arg SERVICE.
#
# Phase 1 of consolidation keeps the nine deployables the platform already has;
# only their source moved. So this file still produces nine images with the same
# names and the same scratch/non-root shape as the nine repositories did --
# what changed is that they are built from one module instead of nine.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder
RUN apk add --no-cache git ca-certificates tzdata
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY . .
ARG SERVICE=api-gateway
ARG TARGETARCH
ARG VERSION=dev
ARG BUILD_TIME
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build \
      -trimpath \
      -ldflags="-s -w -X main.Version=${VERSION} -X main.BuildTime=${BUILD_TIME}" \
      -o /out/server ./cmd/${SERVICE}

FROM scratch
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=builder /etc/passwd /etc/passwd
COPY --from=builder /out/server /server
USER 65532:65532
ENV TZ=UTC
EXPOSE 8080 9090
ENTRYPOINT ["/server"]
