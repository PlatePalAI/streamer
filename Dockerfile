# Multi-stage build for ARM64 (Raspberry Pi 5)
FROM golang:1.25-alpine AS builder

# Install build dependencies
RUN apk add --no-cache \
    gcc \
    musl-dev \
    libjpeg-turbo-dev \
    libjpeg-turbo-static \
    linux-headers

# Set working directory
WORKDIR /build

# Copy go mod files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY . .

# Build the binary with static linking
# -extldflags "-static" ensures all C libraries are statically linked
RUN CGO_ENABLED=1 go build \
    -ldflags="-s -w -extldflags '-static'" \
    -tags 'netgo osusergo' \
    -o streamer main.go
