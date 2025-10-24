# PlatePalAI Streamer

A high-performance Go-based video streaming service that captures video from V4L2 USB cameras and streams it over HTTP using MJPEG. Optimized for efficiency using libjpeg-turbo DCT scaling to provide both full-resolution capture and lightweight SD streaming.

## Features

- **High-Resolution Capture**: Captures video from USB cameras at up to 4K resolution (3840x2160) via V4L2
- **Efficient Streaming**: Streams video at SD resolution (480x270) using MJPEG over HTTP for low bandwidth consumption
- **DCT Scaling**: Uses libjpeg-turbo's DCT scaling for ultra-fast image resizing during JPEG decode
- **Dual-Resolution Buffering**: Maintains both full-resolution and SD frames in memory
- **Command Interface**: Accepts stdin commands for capturing full-resolution snapshots
- **Elixir Integration**: Designed to work as a PORT process with Elixir applications using specific exit codes

## Requirements

- Go 1.25.3 or higher
- V4L2-compatible USB camera
- Linux operating system (V4L2 support required)
- libjpeg-turbo library

## Installation

1. Clone the repository:
```bash
git clone <repository-url>
cd streamer
```

2. Install dependencies:
```bash
go mod download
```

3. Build the binary:
```bash
go build -o streamer
```

## Usage

### Basic Usage

Run with default settings (captures at 3840x2160, streams at 480x270):
```bash
./streamer
```

### Custom Resolution

Specify custom capture resolution:
```bash
./streamer -width 1920 -height 1080
```

### Command Line Flags

- `-width`: Capture width in pixels (default: 3840)
- `-height`: Capture height in pixels (default: 2160)

### HTTP Streaming Endpoint

Once running, the MJPEG stream is available at:
```
http://localhost:8080/stream
```

View in browser or use with any MJPEG-compatible client.

### Capturing Full-Resolution Frames

Send the `CAPTURE` command to stdin to save a full-resolution frame:
```bash
echo "CAPTURE" | ./streamer
```

Frames are saved to `~/Desktop/frame.jpeg` with the full capture resolution.

## Architecture

### Components

- **Frame Capture Loop**: Continuously captures frames from the V4L2 device using go4vl
- **Frame Buffer**: Thread-safe double buffer storing both full-resolution and SD frames
- **HTTP Server**: Serves MJPEG stream at ~30fps (66ms intervals)
- **Stdin Listener**: Processes commands for frame capture and control

### Performance Optimization

The streamer uses libjpeg-turbo's DCT scaling feature, which resizes images during the JPEG decode process rather than decoding the full image first. This is significantly faster than traditional decode-then-resize approaches.

## Exit Codes

The application uses specific exit codes for integration with Elixir PORT monitoring:

- `0`: Normal exit
- `1`: Generic error
- `2`: USB device error (device not available or disconnected)

## Device Configuration

The default device path is `/dev/video0`. To use a different device, modify the `devicePath` constant in `main.go`.

## Dependencies

- [go4vl](https://github.com/vladimirvivien/go4vl): V4L2 bindings for Go
- [go-libjpeg](https://github.com/pixiv/go-libjpeg): libjpeg-turbo bindings for efficient JPEG operations

## Troubleshooting

### Device Access Issues

Ensure your user has permission to access the video device:
```bash
sudo usermod -a -G video $USER
```

Log out and back in for changes to take effect.

### Device Not Found

List available V4L2 devices:
```bash
v4l2-ctl --list-devices
```

### Performance Issues

- Reduce capture resolution with `-width` and `-height` flags
- Ensure libjpeg-turbo is properly installed for optimal performance
- Check camera USB connection and use a USB 3.0 port if available

## License

Part of the PlatePalAI project.
