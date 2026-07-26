FROM golang:1.26-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /bin/blink-alert-merger ./cmd/alert_merger

FROM alpine:3.21
COPY --from=builder /bin/blink-alert-merger /bin/blink-alert-merger
ENTRYPOINT ["/bin/blink-alert-merger"]
