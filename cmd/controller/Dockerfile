FROM golang:1.26-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /bin/blink-controller ./cmd/controller

FROM alpine:3.21
COPY --from=builder /bin/blink-controller /bin/blink-controller
EXPOSE 8080
ENTRYPOINT ["/bin/blink-controller"]
