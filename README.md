# Building for Raspberry Pi 5

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

### Option 1: SCP (Secure Copy)

```bash
scp ./bin/streamer pi@<rpi-ip>:~/
```

Then on your RPI5:

```bash
chmod +x ~/streamer
sudo ./streamer -width 3840 -height 2160
```

### Option 2: Using rsync

```bash
rsync -avz ./bin/streamer pi@<rpi-ip>:~/
```

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

## Development Notes

- The Dockerfile uses Go 1.25 to match your go.mod version
- Binary is statically linked with all dependencies embedded (~8MB)
- Binary is stripped (`-ldflags="-s -w"`) to reduce size
- CGO is required for `go-libjpeg` and `go4vl` dependencies
- **No packages need to be installed on the RPI5** - the binary is completely self-contained
