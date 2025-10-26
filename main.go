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
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

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

var cam *device.Device

func main() {
	flag.Parse()

	// Ensure all log output goes to stderr (data output goes to stdout)
	log.SetOutput(os.Stderr)

	log.Printf("Starting v4l2 MJPEG streamer with libjpeg-turbo DCT scaling...")

	frameBuffer := &FrameBuffer{}

	var err error
	cam, err = device.Open(devicePath, device.WithBufferSize(4))
	if err != nil {
		log.Printf("FATAL: Failed to open USB device: %v", err)
		os.Exit(ExitCodeUSBError)
	}
	defer cam.Close()
	log.Println("Device opened with 4 buffers")

	// Auto-detect best MJPEG resolution if width/height not specified
	captureWidth := *widthCapture
	captureHeight := *heightCapture

	if captureWidth == 0 || captureHeight == 0 {
		log.Println("Auto-detecting best MJPEG resolution...")
		captureWidth, captureHeight, err = getBestMJPEGResolution(cam)
		if err != nil {
			log.Printf("FATAL: Failed to detect MJPEG resolution: %v", err)
			os.Exit(ExitCodeUSBError)
		}
		log.Printf("Auto-detected resolution: %dx%d", captureWidth, captureHeight)
	} else {
		log.Printf("Using specified resolution: %dx%d", captureWidth, captureHeight)
	}

	// Capture at specified resolution (MJPEG)
	if err := cam.SetPixFormat(v4l2.PixFormat{
		Width:       uint32(captureWidth),
		Height:      uint32(captureHeight),
		PixelFormat: v4l2.PixelFmtMJPEG,
		Field:       v4l2.FieldNone,
	}); err != nil {
		log.Printf("FATAL: Failed to set pixel format: %v", err)
		os.Exit(ExitCodeUSBError)
	}

	pixFmt, err := cam.GetPixFormat()
	if err != nil {
		log.Fatalf("Failed to get pixel format: %v", err)
	}
	log.Printf("Capture format: %dx%d %s", pixFmt.Width, pixFmt.Height, pixFmt.PixelFormat)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cam.GetFrames()

	if err := cam.Start(ctx); err != nil {
		log.Printf("FATAL: Failed to start stream: %v", err)
		os.Exit(ExitCodeUSBError)
	}
	log.Println("Stream started successfully")

	go captureFrames(ctx, cam, frameBuffer)
	go startHTTPServer(frameBuffer)

	listenStdin(frameBuffer)
}

func captureFrames(ctx context.Context, cam *device.Device, frameBuffer *FrameBuffer) {
	log.Println("Starting frame capture...")
	frameChan := cam.GetFrames()

	for {
		select {
		case <-ctx.Done():
			return
		case frame, ok := <-frameChan:
			if !ok {
				log.Println("FATAL: Frame channel closed - USB device disconnected")
				os.Exit(ExitCodeUSBError)
			}
			if frame == nil {
				continue
			}

			// Resize full resolution MJPEG to SD using DCT scaling
			resizedFrame, err := resizeMJPEGTurbo(frame.Data, widthSD, heightSD)
			if err != nil {
				log.Printf("Failed to resize frame: %v", err)
				frame.Release()
				continue
			}

			// Store raw full resolution MJPEG and resized SD MJPEG
			frameBuffer.Update(frame.Data, resizedFrame)

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
	if err := stdjpeg.Encode(&buf, img, &stdjpeg.Options{Quality: 40}); err != nil {
		return nil, fmt.Errorf("encode error: %w", err)
	}

	return buf.Bytes(), nil
}

func startHTTPServer(frameBuffer *FrameBuffer) {
	http.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("Client connected: %s", r.RemoteAddr)

		w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=frame")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "close")

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
			return
		}

		ticker := time.NewTicker(66 * time.Millisecond) // ~30 fps
		defer ticker.Stop()

		for range ticker.C {
			frame := frameBuffer.GetSD() // Stream SD version
			if frame == nil {
				continue
			}

			fmt.Fprintf(w, "--frame\r\n")
			fmt.Fprintf(w, "Content-Type: image/jpeg\r\n")
			fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", len(frame))
			if _, err := w.Write(frame); err != nil {
				log.Printf("Client disconnected: %s", r.RemoteAddr)
				return
			}
			fmt.Fprintf(w, "\r\n")
			flusher.Flush()
		}
	})

	log.Println("HTTP server starting on :8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatalf("HTTP server failed: %v", err)
	}
}

func listenStdin(frameBuffer *FrameBuffer) {
	log.Println("Listening for commands on stdin (type 'CAPTURE' to save full resolution frame, 'LIST' to list devices, 'INFO' for device info, 'CONTROLS' for JSON controls, 'SET_CONTROL <ID> <value>')...")
	scanner := bufio.NewScanner(os.Stdin)

	for scanner.Scan() {
		command := strings.TrimSpace(scanner.Text())
		parts := strings.Fields(command)

		if len(parts) == 0 {
			continue
		}

		if parts[0] == "CAPTURE" {
			log.Println("CAPTURE command received")
			saveFrame(frameBuffer)
		} else if parts[0] == "LIST" {
			log.Println("LIST command received")
			listDevices()
		} else if parts[0] == "INFO" {
			log.Println("INFO command received")
			showDeviceInfo()
		} else if parts[0] == "CONTROLS" {
			log.Println("CONTROLS command received")
			getControlsJSON()
		} else if parts[0] == "SET_CONTROL" {
			if len(parts) != 3 {
				log.Printf("SET_CONTROL command received with invalid arguments: %s", command)
				outputJSON(map[string]interface{}{
					"status": "error",
					"error":  "invalid command format, expected: SET_CONTROL <ID> <value>",
				})
			} else {
				log.Printf("SET_CONTROL command received: ID=%s Value=%s", parts[1], parts[2])
				setControl(parts[1], parts[2])
			}
		}
	}

	if err := scanner.Err(); err != nil {
		log.Printf("Error reading stdin: %v", err)
	}
}

func saveFrame(frameBuffer *FrameBuffer) {
	// Get raw full resolution MJPEG from camera (no processing)
	frame := frameBuffer.GetFull()
	if frame == nil {
		log.Println("No frame available to save")
		return
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		log.Printf("Failed to get home directory: %v", err)
		return
	}

	desktopPath := filepath.Join(homeDir, "Desktop", "frame.jpeg")

	// Save raw full resolution MJPEG directly
	if err := os.WriteFile(desktopPath, frame, 0644); err != nil {
		log.Printf("Failed to save frame: %v", err)
		return
	}

	log.Printf("Full resolution frame saved to: %s", desktopPath)
}

func listDevices() {
	log.Println("Enumerating connected v4l2 devices...")

	devices, err := device.GetAllDevicePaths()
	if err != nil {
		log.Printf("ERROR: Failed to enumerate devices: %v", err)
		return
	}

	if len(devices) == 0 {
		log.Println("No v4l2 devices found")
		return
	}

	log.Printf("Found %d device(s):", len(devices))
	for i, devPath := range devices {
		// Try to open device and get capability info
		tempDev, err := device.Open(devPath)
		if err != nil {
			log.Printf("  [%d] %s - (unable to open: %v)", i, devPath, err)
			continue
		}

		cap := tempDev.Capability()
		tempDev.Close()

		log.Printf("  [%d] %s", i, devPath)
		log.Printf("      Card: %s", cap.Card)
		log.Printf("      Driver: %s", cap.Driver)
		log.Printf("      Bus: %s", cap.BusInfo)
	}
}

func showDeviceInfo() {
	if cam == nil {
		log.Println("ERROR: No device is currently open")
		return
	}

	log.Println("=== Device Information ===")

	// Get format descriptions
	log.Println("\n--- Supported Formats ---")
	formats, err := cam.GetFormatDescriptions()
	if err != nil {
		log.Printf("ERROR: Failed to get format descriptions: %v", err)
	} else {
		log.Printf("Found %d format(s):", len(formats))
		for i, fmt := range formats {
			log.Printf("  [%d] %+v", i, fmt)
		}
	}

	// Get all frame sizes for all formats
	log.Println("\n--- Frame Sizes (Resolutions) ---")
	frameSizes, err := v4l2.GetAllFormatFrameSizes(cam.Fd())
	if err != nil {
		log.Printf("ERROR: Failed to get frame sizes: %v", err)
	} else {
		log.Printf("Found %d frame size configuration(s):", len(frameSizes))
		for i, fs := range frameSizes {
			log.Printf("  [%d] %+v", i, fs)
		}
	}

	// Get all controls
	log.Println("\n--- Available Controls ---")
	controls, err := cam.QueryAllControls()
	if err != nil {
		log.Printf("ERROR: Failed to query controls: %v", err)
	} else {
		log.Printf("Found %d control(s):", len(controls))
		for i, ctrl := range controls {
			log.Printf("  [%d] %+v", i, ctrl)
		}
	}

	log.Println("\n=== End Device Information ===")
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
		log.Println("ERROR: No device is currently open")
		return
	}

	controls, err := cam.QueryAllControls()
	if err != nil {
		log.Printf("ERROR: Failed to query controls: %v", err)
		return
	}

	// Convert controls to JSON
	jsonData, err := json.Marshal(controls)
	if err != nil {
		log.Printf("ERROR: Failed to marshal controls to JSON: %v", err)
		return
	}

	// Output JSON to stdout (not stderr like log does)
	fmt.Println(string(jsonData))
}

func outputJSON(data map[string]interface{}) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		log.Printf("ERROR: Failed to marshal JSON: %v", err)
		return
	}
	fmt.Println(string(jsonData))
}

func setControl(idStr string, valueStr string) {
	if cam == nil {
		log.Println("ERROR: No device is currently open")
		outputJSON(map[string]interface{}{
			"status": "error",
			"error":  "no device is currently open",
		})
		return
	}

	// Parse control ID
	var controlID uint32
	if _, err := fmt.Sscanf(idStr, "%d", &controlID); err != nil {
		log.Printf("ERROR: Invalid control ID: %s", idStr)
		outputJSON(map[string]interface{}{
			"status": "error",
			"error":  fmt.Sprintf("invalid control ID: %s", idStr),
		})
		return
	}

	// Parse control value
	var value int32
	if _, err := fmt.Sscanf(valueStr, "%d", &value); err != nil {
		log.Printf("ERROR: Invalid control value: %s", valueStr)
		outputJSON(map[string]interface{}{
			"status": "error",
			"error":  fmt.Sprintf("invalid control value: %s", valueStr),
		})
		return
	}

	// Set the control value
	if err := cam.SetControlValue(controlID, v4l2.CtrlValue(value)); err != nil {
		log.Printf("ERROR: Failed to set control %d to %d: %v", controlID, value, err)
		outputJSON(map[string]interface{}{
			"status": "error",
			"error":  fmt.Sprintf("failed to set control: %v", err),
			"id":     controlID,
			"value":  value,
		})
		return
	}

	log.Printf("Successfully set control %d to %d", controlID, value)
	outputJSON(map[string]interface{}{
		"status": "success",
		"id":     controlID,
		"value":  value,
	})
}
