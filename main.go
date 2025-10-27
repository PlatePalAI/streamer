package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	stdjpeg "image/jpeg"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/pixiv/go-libjpeg/jpeg"
	"github.com/vladimirvivien/go4vl/device"
	"github.com/vladimirvivien/go4vl/v4l2"
)

const (
	devicePath = "/dev/video0"
	widthSD    = 480
	heightSD   = 270

	// Exit codes for Elixir PORT monitoring
	ExitCodeNormal       = 0
	ExitCodeGenericError = 1
	ExitCodeUSBError     = 2 // USB device not available or disconnected
)

var (
	widthCapture  = flag.Int("width", 0, "Capture width in pixels (0 = auto-detect best MJPEG resolution)")
	heightCapture = flag.Int("height", 0, "Capture height in pixels (0 = auto-detect best MJPEG resolution)")
)

type FrameBuffer struct {
	mu               sync.RWMutex
	currentFullFrame []byte // Full resolution MJPEG from camera (raw, no processing)
	currentSDFrame   []byte // Resized SD MJPEG for streaming
}

func (fb *FrameBuffer) Update(frameFull []byte, frameSD []byte) {
	fb.mu.Lock()
	defer fb.mu.Unlock()

	fb.currentFullFrame = make([]byte, len(frameFull))
	copy(fb.currentFullFrame, frameFull)

	if frameSD != nil {
		fb.currentSDFrame = make([]byte, len(frameSD))
		copy(fb.currentSDFrame, frameSD)
	}
}

func (fb *FrameBuffer) GetSD() []byte {
	fb.mu.RLock()
	defer fb.mu.RUnlock()
	if fb.currentSDFrame == nil {
		return nil
	}
	frame := make([]byte, len(fb.currentSDFrame))
	copy(frame, fb.currentSDFrame)
	return frame
}

func (fb *FrameBuffer) GetFull() []byte {
	fb.mu.RLock()
	defer fb.mu.RUnlock()
	if fb.currentFullFrame == nil {
		return nil
	}
	frame := make([]byte, len(fb.currentFullFrame))
	copy(frame, fb.currentFullFrame)
	return frame
}

var (
	cam                *device.Device
	stdoutMutex        sync.Mutex
	activeClients      int32          // Atomic counter for active HTTP clients
	newFrameNotifyChan chan struct{}  // Buffered channel to broadcast new frame availability to all HTTP handlers
)

func main() {
	flag.Parse()

	logJSON("info", "Starting v4l2 MJPEG streamer with libjpeg-turbo DCT scaling")

	frameBuffer := &FrameBuffer{}
	newFrameNotifyChan = make(chan struct{}, 1) // Buffered to avoid blocking capture thread

	var err error
	cam, err = device.Open(devicePath, device.WithBufferSize(4))
	if err != nil {
		logJSON("error", fmt.Sprintf("Failed to open USB device: %v", err))
		os.Exit(ExitCodeUSBError)
	}
	defer cam.Close()
	logJSON("info", "Device opened with 4 buffers")

	// Auto-detect best MJPEG resolution if width/height not specified
	captureWidth := *widthCapture
	captureHeight := *heightCapture

	if captureWidth == 0 || captureHeight == 0 {
		logJSON("info", "Auto-detecting best MJPEG resolution")
		captureWidth, captureHeight, err = getBestMJPEGResolution(cam)
		if err != nil {
			logJSON("error", fmt.Sprintf("Failed to detect MJPEG resolution: %v", err))
			os.Exit(ExitCodeUSBError)
		}
		logJSON("info", fmt.Sprintf("Auto-detected resolution: %dx%d", captureWidth, captureHeight))
	} else {
		logJSON("info", fmt.Sprintf("Using specified resolution: %dx%d", captureWidth, captureHeight))
	}

	// Capture at specified resolution (MJPEG)
	if err := cam.SetPixFormat(v4l2.PixFormat{
		Width:       uint32(captureWidth),
		Height:      uint32(captureHeight),
		PixelFormat: v4l2.PixelFmtMJPEG,
		Field:       v4l2.FieldNone,
	}); err != nil {
		logJSON("error", fmt.Sprintf("Failed to set pixel format: %v", err))
		os.Exit(ExitCodeUSBError)
	}

	pixFmt, err := cam.GetPixFormat()
	if err != nil {
		logJSON("error", fmt.Sprintf("Failed to get pixel format: %v", err))
		os.Exit(ExitCodeGenericError)
	}
	logJSON("info", fmt.Sprintf("Capture format: %dx%d %s", pixFmt.Width, pixFmt.Height, pixFmt.PixelFormat))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cam.GetFrames()

	if err := cam.Start(ctx); err != nil {
		logJSON("error", fmt.Sprintf("Failed to start stream: %v", err))
		os.Exit(ExitCodeUSBError)
	}
	logJSON("info", "Stream started successfully")

	go captureFrames(ctx, cam, frameBuffer)
	go startHTTPServer(frameBuffer)

	listenStdin(frameBuffer)
}

func captureFrames(ctx context.Context, cam *device.Device, frameBuffer *FrameBuffer) {
	logJSON("info", "Starting frame capture")
	frameChan := cam.GetFrames()

	for {
		select {
		case <-ctx.Done():
			return
		case frame, ok := <-frameChan:
			if !ok {
				logJSON("error", "Frame channel closed - USB device disconnected")
				os.Exit(ExitCodeUSBError)
			}
			if frame == nil {
				continue
			}

			// Check if any clients are connected
			clientCount := atomic.LoadInt32(&activeClients)

			if clientCount > 0 {
				// Process frame only when clients are watching
				// Resize full resolution MJPEG to SD using DCT scaling
				resizedFrame, err := resizeMJPEGTurbo(frame.Data, widthSD, heightSD)
				if err != nil {
					logJSON("warning", fmt.Sprintf("Failed to resize frame: %v", err))
					frame.Release()
					continue
				}

				// Store raw full resolution MJPEG and resized SD MJPEG
				frameBuffer.Update(frame.Data, resizedFrame)

				// Notify HTTP handlers that a new frame is available
				// Non-blocking send to avoid slowing down capture
				select {
				case newFrameNotifyChan <- struct{}{}:
				default:
					// Channel already has a notification pending, skip
				}
			}
			// If no clients, just discard the frame (keeps camera buffer flowing)

			frame.Release()
		}
	}
}

func resizeMJPEGTurbo(jpegData []byte, targetWidth, targetHeight int) ([]byte, error) {
	// Decode with DCT scaling - libjpeg-turbo decodes directly to smaller size!
	// This is MUCH faster than decoding full size then resizing
	img, err := jpeg.Decode(bytes.NewReader(jpegData), &jpeg.DecoderOptions{
		ScaleTarget: image.Rect(0, 0, targetWidth, targetHeight),
	})
	if err != nil {
		return nil, fmt.Errorf("turbo decode error: %w", err)
	}

	// No resizing needed! DCT scaling already did it during decode

	// Encode using stdlib
	var buf bytes.Buffer
	if err := stdjpeg.Encode(&buf, img, &stdjpeg.Options{Quality: 80}); err != nil {
		return nil, fmt.Errorf("encode error: %w", err)
	}

	return buf.Bytes(), nil
}

func startHTTPServer(frameBuffer *FrameBuffer) {
	http.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		// Increment active client counter
		count := atomic.AddInt32(&activeClients, 1)
		logJSON("debug", fmt.Sprintf("Client connected: %s (total clients: %d)", r.RemoteAddr, count))

		// Ensure we decrement the counter when this handler exits
		defer func() {
			count := atomic.AddInt32(&activeClients, -1)
			logJSON("debug", fmt.Sprintf("Client disconnected: %s (remaining clients: %d)", r.RemoteAddr, count))
		}()

		w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=frame")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "close")

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
			return
		}

		// Event-driven streaming: wait for notifications of new frames
		for range newFrameNotifyChan {
			frame := frameBuffer.GetSD() // Stream SD version
			if frame == nil {
				continue
			}

			fmt.Fprintf(w, "--frame\r\n")
			fmt.Fprintf(w, "Content-Type: image/jpeg\r\n")
			fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", len(frame))
			if _, err := w.Write(frame); err != nil {
				// Client disconnected (write failed)
				return
			}
			fmt.Fprintf(w, "\r\n")
			flusher.Flush()
		}
	})

	logJSON("info", "HTTP server starting on :8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		logJSON("error", fmt.Sprintf("HTTP server failed: %v", err))
		os.Exit(ExitCodeGenericError)
	}
}

func listenStdin(frameBuffer *FrameBuffer) {
	logJSON("info", "Listening for commands on stdin")
	scanner := bufio.NewScanner(os.Stdin)

	for scanner.Scan() {
		command := strings.TrimSpace(scanner.Text())
		parts := strings.Fields(command)

		if len(parts) == 0 {
			continue
		}

		if parts[0] == "CAPTURE" {
			logJSON("debug", "CAPTURE command received")
			saveFrame(frameBuffer)
		} else if parts[0] == "INFO" {
			logJSON("debug", "INFO command received")
			showDeviceInfo()
		} else if parts[0] == "CONTROLS" {
			logJSON("debug", "CONTROLS command received")
			getControlsJSON()
		} else if parts[0] == "SET_CONTROL" {
			if len(parts) != 3 {
				logJSON("warning", fmt.Sprintf("SET_CONTROL command received with invalid arguments: %s", command))
				writeJSON("set_control_response", map[string]interface{}{
					"status": "error",
					"error":  "invalid command format, expected: SET_CONTROL <ID> <value>",
				})
			} else {
				logJSON("debug", fmt.Sprintf("SET_CONTROL command received: ID=%s Value=%s", parts[1], parts[2]))
				setControl(parts[1], parts[2])
			}
		}
	}

	if err := scanner.Err(); err != nil {
		logJSON("error", fmt.Sprintf("Error reading stdin: %v", err))
	}
}

func saveFrame(frameBuffer *FrameBuffer) {
	// Get raw full resolution MJPEG from camera (no processing)
	frame := frameBuffer.GetFull()
	if frame == nil {
		logJSON("warning", "No frame available to save")
		return
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		logJSON("error", fmt.Sprintf("Failed to get home directory: %v", err))
		return
	}

	desktopPath := filepath.Join(homeDir, "Desktop", "frame.jpeg")

	// Save raw full resolution MJPEG directly
	if err := os.WriteFile(desktopPath, frame, 0644); err != nil {
		logJSON("error", fmt.Sprintf("Failed to save frame: %v", err))
		return
	}

	logJSON("info", fmt.Sprintf("Full resolution frame saved to: %s", desktopPath))
}

func showDeviceInfo() {
	if cam == nil {
		logJSON("error", "No device is currently open")
		return
	}

	logJSON("info", "=== Device Information ===")

	// Get format descriptions
	logJSON("info", "--- Supported Formats ---")
	formats, err := cam.GetFormatDescriptions()
	if err != nil {
		logJSON("error", fmt.Sprintf("Failed to get format descriptions: %v", err))
	} else {
		logJSON("info", fmt.Sprintf("Found %d format(s)", len(formats)))
		for i, format := range formats {
			logJSON("info", fmt.Sprintf("  [%d] %+v", i, format))
		}
	}

	// Get all frame sizes for all formats
	logJSON("info", "--- Frame Sizes (Resolutions) ---")
	frameSizes, err := v4l2.GetAllFormatFrameSizes(cam.Fd())
	if err != nil {
		logJSON("error", fmt.Sprintf("Failed to get frame sizes: %v", err))
	} else {
		logJSON("info", fmt.Sprintf("Found %d frame size configuration(s)", len(frameSizes)))
		for i, fs := range frameSizes {
			logJSON("info", fmt.Sprintf("  [%d] %+v", i, fs))
		}
	}

	// Get all controls
	logJSON("info", "--- Available Controls ---")
	controls, err := cam.QueryAllControls()
	if err != nil {
		logJSON("error", fmt.Sprintf("Failed to query controls: %v", err))
	} else {
		logJSON("info", fmt.Sprintf("Found %d control(s)", len(controls)))
		for i, ctrl := range controls {
			logJSON("info", fmt.Sprintf("  [%d] %+v", i, ctrl))
		}
	}

	logJSON("info", "=== End Device Information ===")
}

func getBestMJPEGResolution(cam *device.Device) (int, int, error) {
	// Get all frame sizes for MJPEG format
	frameSizes, err := v4l2.GetFormatFrameSizes(cam.Fd(), v4l2.PixelFmtMJPEG)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to get MJPEG frame sizes: %w", err)
	}

	if len(frameSizes) == 0 {
		return 0, 0, fmt.Errorf("no MJPEG resolutions available on this device")
	}

	// Find the highest resolution (max width × height)
	var bestWidth, bestHeight uint32
	var maxPixels uint32

	for _, fs := range frameSizes {
		// Use MaxWidth and MaxHeight as they represent the actual resolution
		// (for discrete sizes, Min == Max)
		width := fs.Size.MaxWidth
		height := fs.Size.MaxHeight
		pixels := width * height

		if pixels > maxPixels {
			maxPixels = pixels
			bestWidth = width
			bestHeight = height
		}
	}

	if bestWidth == 0 || bestHeight == 0 {
		return 0, 0, fmt.Errorf("invalid resolution detected: %dx%d", bestWidth, bestHeight)
	}

	return int(bestWidth), int(bestHeight), nil
}

func getControlsJSON() {
	if cam == nil {
		logJSON("error", "No device is currently open")
		return
	}

	// First get all available controls (metadata)
	controls, err := cam.QueryAllControls()
	if err != nil {
		logJSON("error", fmt.Sprintf("Failed to query controls: %v", err))
		return
	}

	// Now query each control individually to get its current value
	controlsWithValues := make([]v4l2.Control, 0, len(controls))
	for _, ctrl := range controls {
		// Get the current value for this control
		currentCtrl, err := cam.GetControl(ctrl.ID)
		if err != nil {
			// Skip control class headers and other unreadable controls (permission denied)
			// These are organizational groupings like "User Controls" or "Camera Controls"
			if strings.Contains(err.Error(), "permission denied") {
				logJSON("debug", fmt.Sprintf("Skipping control class header: %d (%s)", ctrl.ID, ctrl.Name))
				continue
			}
			// For other errors, log warning and use original control info
			logJSON("warning", fmt.Sprintf("Failed to get current value for control %d (%s): %v", ctrl.ID, ctrl.Name, err))
			controlsWithValues = append(controlsWithValues, ctrl)
		} else {
			controlsWithValues = append(controlsWithValues, currentCtrl)
		}
	}

	// Output controls as JSON with type field
	writeJSON("controls", map[string]interface{}{
		"data": controlsWithValues,
	})
}

func writeJSON(msgType string, data map[string]interface{}) {
	stdoutMutex.Lock()
	defer stdoutMutex.Unlock()

	msg := map[string]interface{}{"type": msgType}
	for k, v := range data {
		msg[k] = v
	}

	if err := json.NewEncoder(os.Stdout).Encode(msg); err != nil {
		// Fallback to stderr if JSON encoding fails
		fmt.Fprintf(os.Stderr, "FATAL: Failed to encode JSON: %v\n", err)
	}
}

func logJSON(level, message string, extraFields ...map[string]interface{}) {
	data := map[string]interface{}{
		"level":   level,
		"message": message,
	}

	// Merge any extra fields
	if len(extraFields) > 0 {
		for k, v := range extraFields[0] {
			data[k] = v
		}
	}

	writeJSON("log", data)
}

func setControl(idStr string, valueStr string) {
	if cam == nil {
		logJSON("error", "No device is currently open")
		writeJSON("set_control_response", map[string]interface{}{
			"status": "error",
			"error":  "no device is currently open",
		})
		return
	}

	// Parse control ID
	var controlID uint32
	if _, err := fmt.Sscanf(idStr, "%d", &controlID); err != nil {
		logJSON("error", fmt.Sprintf("Invalid control ID: %s", idStr))
		writeJSON("set_control_response", map[string]interface{}{
			"status": "error",
			"error":  fmt.Sprintf("invalid control ID: %s", idStr),
		})
		return
	}

	// Parse control value
	var value int32
	if _, err := fmt.Sscanf(valueStr, "%d", &value); err != nil {
		logJSON("error", fmt.Sprintf("Invalid control value: %s", valueStr))
		writeJSON("set_control_response", map[string]interface{}{
			"status": "error",
			"error":  fmt.Sprintf("invalid control value: %s", valueStr),
		})
		return
	}

	// Set the control value
	if err := cam.SetControlValue(controlID, v4l2.CtrlValue(value)); err != nil {
		logJSON("error", fmt.Sprintf("Failed to set control %d to %d: %v", controlID, value, err))
		writeJSON("set_control_response", map[string]interface{}{
			"status": "error",
			"error":  fmt.Sprintf("failed to set control: %v", err),
			"id":     controlID,
			"value":  value,
		})
		return
	}

	// Query the control to get the actual value that was set (hardware may clamp to valid range)
	actualControl, err := cam.GetControl(controlID)
	if err != nil {
		logJSON("warning", fmt.Sprintf("Control %d was set but failed to read back value: %v", controlID, err))
		// Still report success but with the requested value
		writeJSON("set_control_response", map[string]interface{}{
			"status": "success",
			"id":     controlID,
			"value":  value,
		})
		return
	}

	logJSON("info", fmt.Sprintf("Successfully set control %d to %d (actual: %d)", controlID, value, actualControl.Value))
	writeJSON("set_control_response", map[string]interface{}{
		"status":  "success",
		"id":      controlID,
		"value":   actualControl.Value,
		"control": actualControl,
	})
}
