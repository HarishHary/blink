FROM golang:1.26-alpine AS builder
RUN apk add --no-cache gcc musl-dev
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO_ENABLED=1: mattn/go-sqlite3 (internal/backends) needs cgo - a stub driver panics at runtime
# without it. Building against musl (this image) keeps the binary compatible with the alpine
# runtime image below.
RUN CGO_ENABLED=1 GOOS=linux go build -trimpath -o /bin/blink-controller ./cmd/controller

FROM alpine:3.21
COPY --from=builder /bin/blink-controller /bin/blink-controller
EXPOSE 8080
ENTRYPOINT ["/bin/blink-controller"]
