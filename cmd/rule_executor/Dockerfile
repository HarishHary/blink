FROM golang:1.26-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /bin/blink-rule-executor ./cmd/rule_executor

FROM alpine:3.21
COPY --from=builder /bin/blink-rule-executor /bin/blink-rule-executor
ENTRYPOINT ["/bin/blink-rule-executor"]
