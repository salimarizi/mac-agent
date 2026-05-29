// Package capture grabs macOS screen frames via Core Graphics
// (CGDisplayCreateImage) and H.264-encodes them with ffmpeg.
//
// Using CGDisplayCreateImage instead of AVFoundation's AVCaptureScreenInput
// sidesteps a dynamic-linking issue in Homebrew-compiled ffmpeg on macOS 14+
// where AVCaptureScreenInput is silently unavailable, causing a gray/black
// capture. CGDisplayCreateImage is a lower-level API that works as long as
// the process (or its Terminal parent) has Screen Recording permission.
//
// Pipeline:
//
//	Go (CGDisplayCreateImage) → raw BGRA frames → ffmpeg stdin
//	ffmpeg stdout → H.264 Annex-B → pion TrackLocalStaticSample

package capture

/*
#cgo LDFLAGS: -framework CoreGraphics -framework CoreFoundation
#include <CoreGraphics/CoreGraphics.h>
#include <dlfcn.h>
#include <stdlib.h>

// CGDisplayCreateImage is marked unavailable in the macOS 15 SDK (Apple wants
// developers to use ScreenCaptureKit). The symbol still exists at runtime, so
// we load it dynamically to bypass the compile-time availability check.
typedef CGImageRef (*CGDisplayCreateImageFn)(CGDirectDisplayID);

static CGImageRef displayCreateImage(CGDirectDisplayID displayID) {
    static CGDisplayCreateImageFn fn = NULL;
    if (!fn) {
        fn = (CGDisplayCreateImageFn)dlsym(RTLD_DEFAULT, "CGDisplayCreateImage");
    }
    return fn ? fn(displayID) : NULL;
}

// captureFrame captures the display at position idx in CGGetActiveDisplayList
// order (0 = main display). The image is drawn into a BGRA bitmap at the
// requested output size; if targetW == 0 the native resolution is used.
// Returns a malloc'd buffer the caller must free(), sets *outW / *outH.
static unsigned char* captureFrame(int idx, int targetW, int* outW, int* outH) {
    uint32_t total = 0;
    CGGetActiveDisplayList(0, NULL, &total);
    if (!total || (uint32_t)idx >= total) return NULL;

    CGDirectDisplayID ids[64];
    CGGetActiveDisplayList(total, ids, &total);

    CGImageRef img = displayCreateImage(ids[idx]);
    if (!img) return NULL;

    int nw = (int)CGImageGetWidth(img);
    int nh = (int)CGImageGetHeight(img);

    int rw, rh;
    if (targetW > 0 && targetW < nw) {
        rw = targetW;
        rh = nh * targetW / nw;
        if (rh & 1) rh++; // keep even for yuv420p
    } else {
        rw = nw;
        rh = nh;
    }
    *outW = rw;
    *outH = rh;

    unsigned char* buf = (unsigned char*)malloc(rw * rh * 4);
    if (!buf) { CGImageRelease(img); return NULL; }

    CGColorSpaceRef cs = CGColorSpaceCreateDeviceRGB();
    CGContextRef ctx = CGBitmapContextCreate(buf, rw, rh, 8, rw * 4, cs,
        (CGBitmapInfo)(kCGImageAlphaNoneSkipFirst | kCGBitmapByteOrder32Little));
    CGColorSpaceRelease(cs);
    if (!ctx) { free(buf); CGImageRelease(img); return NULL; }

    CGContextDrawImage(ctx, CGRectMake(0, 0, rw, rh), img);
    CGContextRelease(ctx);
    CGImageRelease(img);
    return buf;
}
*/
import "C"

import (
	"context"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"github.com/pion/webrtc/v4/pkg/media/h264reader"
)

// Config controls the capture pipeline.
type Config struct {
	// ScreenIndex is the CGGetActiveDisplayList index: "0" = main display,
	// "1" = second display, etc.
	ScreenIndex string
	FPS         int
	Width       int    // scaled output width; 0 = native resolution
	Bitrate     string // e.g. "6M"
	FFmpegPath  string
}

func (c Config) withDefaults() Config {
	if c.FPS == 0 {
		c.FPS = 30
	}
	if c.Bitrate == "" {
		c.Bitrate = "6M"
	}
	if c.FFmpegPath == "" {
		c.FFmpegPath = "ffmpeg"
	}
	if c.ScreenIndex == "" {
		c.ScreenIndex = "0"
	}
	return c
}

// hasVideoToolbox probes whether ffmpeg supports h264_videotoolbox.
func hasVideoToolbox(ffmpegPath string) bool {
	out, err := exec.Command(ffmpegPath, "-hide_banner", "-encoders").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "h264_videotoolbox")
}

// Start captures the screen and returns a channel of H.264 access units
// (Annex-B, with start codes). The channel closes when ctx is cancelled.
func Start(ctx context.Context, cfg Config) (<-chan []byte, error) {
	cfg = cfg.withDefaults()

	displayIdx, err := strconv.Atoi(cfg.ScreenIndex)
	if err != nil {
		return nil, fmt.Errorf("invalid screen index %q: %w", cfg.ScreenIndex, err)
	}

	// Probe dimensions with a test frame before starting ffmpeg.
	var cw, ch C.int
	probe := C.captureFrame(C.int(displayIdx), C.int(cfg.Width), &cw, &ch)
	if probe == nil {
		return nil, fmt.Errorf(
			"CGDisplayCreateImage failed for display index %d — "+
				"grant Screen Recording to Terminal and restart it", displayIdx)
	}
	C.free(unsafe.Pointer(probe))

	frameW := int(cw)
	frameH := int(ch)

	// Pick encoder: prefer VideoToolbox hardware, fall back to libx264.
	useHW := hasVideoToolbox(cfg.FFmpegPath)

	var args []string
	if useHW {
		args = []string{
			"-hide_banner", "-loglevel", "error",
			"-probesize", "32",
			"-analyzeduration", "0",
			"-f", "rawvideo",
			"-pixel_format", "bgra",
			"-video_size", fmt.Sprintf("%dx%d", frameW, frameH),
			"-framerate", fmt.Sprintf("%d", cfg.FPS),
			"-i", "pipe:0",
			"-vf", "format=nv12",
			"-c:v", "h264_videotoolbox",
			"-realtime", "true",
			"-profile:v", "baseline",
			"-pix_fmt", "yuv420p",
			"-g", fmt.Sprintf("%d", cfg.FPS), // keyframe every 1s
			"-b:v", cfg.Bitrate,
			"-maxrate", cfg.Bitrate,
			"-bufsize", bitrateHalf(cfg.Bitrate),
			"-bsf:v", "h264_metadata=aud=insert",
			"-flush_packets", "1",
			"-f", "h264",
			"pipe:1",
		}
		log.Print("capture: using VideoToolbox hardware encoder")
	} else {
		args = []string{
			"-hide_banner", "-loglevel", "error",
			"-probesize", "32",
			"-analyzeduration", "0",
			"-f", "rawvideo",
			"-pixel_format", "bgra",
			"-video_size", fmt.Sprintf("%dx%d", frameW, frameH),
			"-framerate", fmt.Sprintf("%d", cfg.FPS),
			"-i", "pipe:0",
			"-vf", "format=yuv420p",
			"-c:v", "libx264",
			"-preset", "ultrafast",
			"-tune", "zerolatency",
			"-profile:v", "baseline",
			"-pix_fmt", "yuv420p",
			"-g", fmt.Sprintf("%d", cfg.FPS),
			"-keyint_min", fmt.Sprintf("%d", cfg.FPS),
			"-sc_threshold", "0",
			"-x264-params", "repeat-headers=1",
			"-b:v", cfg.Bitrate,
			"-maxrate", cfg.Bitrate,
			"-bufsize", bitrateHalf(cfg.Bitrate),
			"-bsf:v", "h264_metadata=aud=insert",
			"-flush_packets", "1",
			"-f", "h264",
			"pipe:1",
		}
		log.Print("capture: using libx264 software encoder")
	}

	cmd := exec.CommandContext(ctx, cfg.FFmpegPath, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = logWriter{}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ffmpeg: %w", err)
	}
	log.Printf("capture: started (display=%d frame=%dx%d fps=%d bitrate=%s)",
		displayIdx, frameW, frameH, cfg.FPS, cfg.Bitrate)

	// Small buffer: 2 frames max in flight. More = more latency.
	frames := make(chan []byte, 2)

	// Capture goroutine: grab a frame every tick and write raw BGRA to ffmpeg.
	go func() {
		defer stdin.Close()
		ticker := time.NewTicker(time.Second / time.Duration(cfg.FPS))
		defer ticker.Stop()

		frameBuf := make([]byte, frameW*frameH*4)

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				var fw, fh C.int
				cbuf := C.captureFrame(C.int(displayIdx), C.int(cfg.Width), &fw, &fh)
				if cbuf == nil {
					continue
				}
				size := int(fw) * int(fh) * 4
				if size != len(frameBuf) {
					frameBuf = make([]byte, size)
				}
				src := (*[1 << 28]byte)(unsafe.Pointer(cbuf))[:size:size]
				copy(frameBuf, src)
				C.free(unsafe.Pointer(cbuf))

				if _, err := stdin.Write(frameBuf); err != nil {
					return
				}
			}
		}
	}()

	go assemble(stdout, frames)
	go func() {
		_ = cmd.Wait()
		log.Print("capture: ffmpeg exited")
	}()

	return frames, nil
}

// bitrateHalf parses a bitrate string like "6M" and returns half of it.
// Used for bufsize to reduce encoder buffering latency.
func bitrateHalf(b string) string {
	b = strings.TrimSpace(b)
	if len(b) == 0 {
		return "3M"
	}
	suffix := ""
	numStr := b
	last := b[len(b)-1]
	if last == 'M' || last == 'm' || last == 'K' || last == 'k' {
		suffix = string(last)
		numStr = b[:len(b)-1]
	}
	val, err := strconv.ParseFloat(numStr, 64)
	if err != nil {
		return b
	}
	half := val / 2
	if half < 0.1 {
		half = 0.1
	}
	return fmt.Sprintf("%.1f%s", half, suffix)
}

// assemble reads NAL units and groups them into complete H.264 access units,
// using AUD (NAL type 9) as the frame boundary marker.
func assemble(r io.Reader, out chan<- []byte) {
	defer close(out)

	reader, err := h264reader.NewReader(r)
	if err != nil {
		log.Printf("capture: h264reader: %v", err)
		return
	}

	startCode := []byte{0x00, 0x00, 0x00, 0x01}
	var au []byte

	flush := func() {
		if len(au) == 0 {
			return
		}
		frame := make([]byte, len(au))
		copy(frame, au)
		au = au[:0]
		// Non-blocking send: drop frame if consumer is behind.
		select {
		case out <- frame:
		default:
		}
	}

	const nalAUD = 9
	for {
		nal, err := reader.NextNAL()
		if err == io.EOF {
			flush()
			return
		}
		if err != nil {
			log.Printf("capture: NextNAL: %v", err)
			flush()
			return
		}
		if nal.UnitType == nalAUD && len(au) > 0 {
			flush()
		}
		au = append(au, startCode...)
		au = append(au, nal.Data...)
	}
}

type logWriter struct{}

func (logWriter) Write(p []byte) (int, error) {
	log.Printf("ffmpeg: %s", p)
	return len(p), nil
}
