#!/bin/bash
set -e

echo "Building streamer for Raspberry Pi 5 (ARM64)..."

# Create bin directory if it doesn't exist
mkdir -p bin

# Build the Docker image and extract the binary
echo "Step 1/3: Building Docker image..."
docker build --platform linux/arm64 -t streamer-builder .

echo "Step 2/3: Extracting binary..."
# Remove any existing temp container
docker rm -f temp-streamer 2>/dev/null || true
docker create --name temp-streamer streamer-builder
docker cp temp-streamer:/build/streamer ./bin/streamer

echo "Step 3/3: Cleaning up..."
docker rm temp-streamer

echo ""
echo "✓ Build complete!"
echo "  Binary location: ./bin/streamer"
echo ""
echo "To deploy to your RPI5:"
echo "  scp ./bin/streamer pi@<rpi-ip>:~/"
echo ""
