package main

import (
	"bufio"
	"bytes"
	"context"
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
	widthCapture  = flag.Int("width", 3840, "Capture width in pixels")
	heightCapture = flag.Int("height", 2160, "Capture height in pixels")
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

func main() {
	flag.Parse()

	log.Printf("Starting v4l2 MJPEG streamer with libjpeg-turbo DCT scaling...")
	log.Printf("Capture resolution: %dx%d", *widthCapture, *heightCapture)

	frameBuffer := &FrameBuffer{}

	cam, err := device.Open(devicePath, device.WithBufferSize(4))
	if err != nil {
		log.Printf("FATAL: Failed to open USB device: %v", err)
		os.Exit(ExitCodeUSBError)
	}
	defer cam.Close()
	log.Println("Device opened with 4 buffers")

	// Capture at specified resolution (MJPEG)
	if err := cam.SetPixFormat(v4l2.PixFormat{
		Width:       uint32(*widthCapture),
		Height:      uint32(*heightCapture),
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
	log.Println("Listening for commands on stdin (type 'CAPTURE' to save full resolution frame)...")
	scanner := bufio.NewScanner(os.Stdin)

	for scanner.Scan() {
		command := strings.TrimSpace(scanner.Text())

		if command == "CAPTURE" {
			log.Println("CAPTURE command received")
			saveFrame(frameBuffer)
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
