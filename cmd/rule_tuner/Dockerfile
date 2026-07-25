FROM golang:1.26-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /bin/blink-rule-tuner ./cmd/rule_tuner

FROM alpine:3.21
COPY --from=builder /bin/blink-rule-tuner /bin/blink-rule-tuner
ENTRYPOINT ["/bin/blink-rule-tuner"]
