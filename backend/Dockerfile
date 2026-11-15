# syntax=docker/dockerfile:1

# ---------- build ----------
FROM golang:1.25-alpine AS build

WORKDIR /src

# Cache module downloads separately from sources.
COPY go.mod go.sum* ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/restreamd ./cmd/restreamd

# ---------- runtime ----------
FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata \
 && addgroup -g 10001 -S restream \
 && adduser -u 10001 -S -G restream -h /app restream \
 && mkdir -p /data /app \
 && chown -R restream:restream /data /app

COPY --from=build /out/restreamd /usr/local/bin/restreamd

USER restream
WORKDIR /app

ENV RC_DATA_DIR=/data \
    RC_LISTEN=0.0.0.0:8080 \
    TZ=UTC

EXPOSE 8080
VOLUME ["/data"]

HEALTHCHECK --interval=20s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/usr/local/bin/restreamd", "healthcheck"]

ENTRYPOINT ["/usr/local/bin/restreamd"]
CMD ["serve"]
