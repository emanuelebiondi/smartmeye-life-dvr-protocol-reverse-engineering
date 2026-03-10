FROM golang:1.26 AS builder

WORKDIR /src/legacybridge
COPY legacybridge/go.mod legacybridge/go.sum ./
RUN go mod download
COPY legacybridge/ ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/legacybridge .

FROM alexxit/go2rtc:latest

COPY --from=builder /out/legacybridge /usr/local/bin/legacybridge
COPY docker/run_legacybridge.sh /usr/local/bin/run_legacybridge
COPY docker/run_legacyhub.sh /usr/local/bin/run_legacyhub
COPY docker/run_legacysubscriber.sh /usr/local/bin/run_legacysubscriber
RUN chmod +x /usr/local/bin/run_legacybridge /usr/local/bin/run_legacyhub /usr/local/bin/run_legacysubscriber
