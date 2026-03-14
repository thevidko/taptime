# Stage 1: Build the Go binary
FROM golang:1.25-alpine AS builder

WORKDIR /app

# Enable CGO_ENABLED=0 to ensure a statically linked binary
ENV CGO_ENABLED=0
ENV GOOS=linux

# Copy go.mod and go.sum files
COPY go.mod go.sum* ./
RUN go mod download || true

# Copy the source code
COPY . .

# Build the binary
RUN go build -o tap-time .

# Stage 2: Create a minimal image
FROM alpine:latest

RUN apk add --no-cache tzdata

WORKDIR /app

# Copy the built binary from the builder stage
COPY --from=builder /app/tap-time .
# Copy templates
COPY --from=builder /app/templates ./templates

# Define a volume for SQLite database persistence
VOLUME /data
ENV DB_PATH=/data/data.db
ENV PORT=8080

EXPOSE 8080

CMD ["./tap-time"]
