# V4L2 MJPEG Streamer for Raspberry Pi 5

A high-performance MJPEG video streamer for Raspberry Pi 5 with USB camera support. Features hardware-accelerated JPEG processing using libjpeg-turbo, HTTP streaming, frame capture, and camera control via stdin commands.

This project uses Docker to cross-compile from macOS (or any platform) to ARM64/Linux for the Raspberry Pi 5.

## Prerequisites

- **Docker Desktop** installed and running
- Ensure BuildKit is enabled (it's on by default in recent versions)

## Quick Start

Simply run the build script:

```bash
./build.sh
```

This will create `./bin/streamer` - a **statically-linked** binary compiled for your RPI5 (~8MB, fully self-contained with no dependencies required).

## Deploying to RPI5

Copy the binary to your Raspberry Pi 5:

```bash
scp ./bin/streamer pi@<rpi-ip>:~/
```

Then on your RPI5:

```bash
chmod +x ~/streamer
sudo ./streamer -width 3840 -height 2160
```

**Command Line Options:**
- `-width <pixels>` - Capture width (default: 0 for auto-detect)
- `-height <pixels>` - Capture height (default: 0 for auto-detect)

When width and height are set to 0 (or omitted), the streamer automatically detects and uses the highest available MJPEG resolution supported by your camera.

## How It Works

The build process uses Docker to create a statically-linked binary:

1. **Builder stage**: Uses `golang:1.25-alpine` for ARM64
   - Installs gcc, musl-dev, libjpeg-turbo (both dynamic and static), and linux-headers
   - Compiles the Go binary with CGO enabled and static linking (`-extldflags '-static'`)
   - All dependencies (libjpeg, libc, etc.) are embedded directly into the binary

The `build.sh` script:
- Builds the Docker image for ARM64 platform
- Extracts the statically-linked binary from `/build/streamer` to `./bin/`
- Cleans up temporary containers

**Key benefit**: The resulting binary has zero runtime dependencies on the target RPI5!

## Manual Build (Alternative)

If you prefer to run Docker commands manually:

```bash
# Build the image
docker build --platform linux/arm64 -t streamer-builder .

# Extract the binary
docker create --name temp-streamer streamer-builder
docker cp temp-streamer:/build/streamer ./bin/streamer
docker rm temp-streamer
```

## Troubleshooting

### Error: "platform linux/arm64 not found"

Make sure Docker Desktop has multi-platform support enabled:
- Open Docker Desktop settings
- Under "Features in development" or "Experimental features"
- Enable "Use containerd for pulling and storing images"

### Error: "no matching manifest for linux/arm64"

Try using a different base image tag or update Docker Desktop to the latest version.

### Build is slow

The first build will be slower as Docker downloads the ARM64 base image and dependencies. Subsequent builds will be much faster due to layer caching.

### Permission denied when running on RPI5

Make sure to set execute permissions after copying:
```bash
chmod +x streamer
```

Also, accessing `/dev/video0` typically requires root or being in the `video` group:
```bash
sudo usermod -a -G video $USER
# Log out and back in for group changes to take effect
```

## Usage

### HTTP Streaming

Once running, the streamer provides an MJPEG stream at:
```
http://<rpi-ip>:8080/stream
```

You can view this in any browser or media player that supports MJPEG streams. The stream runs at approximately 30 fps at 480x270 resolution (SD).

### Stdin Commands

The streamer accepts commands via stdin for integration with other applications (e.g., Elixir PORT):

- **`CAPTURE`** - Saves the current full-resolution frame to `~/Desktop/frame.jpeg`
- **`INFO`** - Displays device information (supported formats, resolutions, controls)
- **`CONTROLS`** - Returns all camera controls as JSON
- **`SET_CONTROL <ID> <value>`** - Sets a camera control value (e.g., brightness, contrast)

**Example:**
```bash
echo "CAPTURE" | sudo ./streamer
echo "CONTROLS" | sudo ./streamer
echo "SET_CONTROL 9963776 128" | sudo ./streamer  # Set brightness to 128
```

### JSON Output

All output is formatted as JSON for easy parsing:

```json
{"type": "log", "level": "info", "message": "Stream started successfully"}
{"type": "controls", "data": [...]}
{"type": "set_control_response", "status": "success", "id": 9963776, "value": 128}
```

### Exit Codes

- **0** - Normal exit
- **1** - Generic error
- **2** - USB device error (device not available or disconnected)

## Architecture

The application maintains two frame buffers:
1. **Full Resolution Buffer** - Raw MJPEG frames from the camera at capture resolution
2. **SD Buffer** - Downscaled frames (480x270) for HTTP streaming

**Key Features:**
- Uses libjpeg-turbo's DCT scaling for efficient frame resizing during JPEG decode (much faster than decode-then-resize)
- Mutex-protected frame buffers for thread-safe access
- Separate goroutines for capture and HTTP streaming
- Auto-detection of best MJPEG resolution when not specified

## Development Notes

- The Dockerfile uses Go 1.25 to match your go.mod version
- Binary is statically linked with all dependencies embedded (~8MB)
- Binary is stripped (`-ldflags="-s -w"`) to reduce size
- CGO is required for `go-libjpeg` and `go4vl` dependencies
- **No packages need to be installed on the RPI5** - the binary is completely self-contained

### Dependencies
- `github.com/pixiv/go-libjpeg` - libjpeg-turbo bindings for fast JPEG processing
- `github.com/vladimirvivien/go4vl` - V4L2 (Video4Linux2) bindings for camera access
