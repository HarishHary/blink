FROM golang:1.26-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /bin/blink-alert-enricher ./cmd/alert_enricher

FROM alpine:3.21
COPY --from=builder /bin/blink-alert-enricher /bin/blink-alert-enricher
ENTRYPOINT ["/bin/blink-alert-enricher"]
