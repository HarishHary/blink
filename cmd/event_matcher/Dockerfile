FROM golang:1.26-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /bin/blink-event-matcher ./cmd/event_matcher

FROM alpine:3.21
COPY --from=builder /bin/blink-event-matcher /bin/blink-event-matcher
ENTRYPOINT ["/bin/blink-event-matcher"]
