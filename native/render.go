// Package native owns isolated libass renderers. A renderer is not shared by jobs.
package native

/*
#cgo linux LDFLAGS: -ldl
#include <stdlib.h>
#include "render.h"
*/
import "C"

import (
	"bytes"
	"cmp"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"unicode/utf16"
	"unsafe"
)

type Config struct {
	Width, Height               int
	StorageWidth, StorageHeight int
	Fonts                       string
	Library                     string
}

type Event struct {
	Start, Duration int64
	Text, Effect    string
}

type Renderer struct {
	handle              *C.SubtrimRenderer
	config              Config
	font                string
	Events              []Event
	ready               bool
	fontFree            bool
	boundaries          []renderBoundary
	lastFrame           *C.ASS_Image
	frameTime, frameEnd int64
}

type renderBoundary struct {
	time    int64
	dynamic int
}

var timeDependent = regexp.MustCompile(`(?i)\\[ \t]*(?:t|move|fad|k)`)

func Open(data []byte, config Config) (*Renderer, error) {
	if config.Width <= 0 || config.Height <= 0 || config.Width > math.MaxInt32 || config.Height > math.MaxInt32 ||
		config.StorageWidth < 0 || config.StorageHeight < 0 || config.StorageWidth > math.MaxInt32 || config.StorageHeight > math.MaxInt32 ||
		(config.StorageWidth == 0) != (config.StorageHeight == 0) {
		return nil, errors.New("invalid rendering dimensions")
	}
	font := ""
	if config.Fonts != "" {
		entries, err := os.ReadDir(config.Fonts)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			switch strings.ToLower(filepath.Ext(entry.Name())) {
			case ".ttf", ".otf", ".ttc", ".otc":
				font = filepath.Join(config.Fonts, entry.Name())
			}
			if font != "" {
				break
			}
		}
		if font == "" {
			return nil, errors.New("font directory contains no supported font files")
		}
	}
	path := config.Library
	if path == "" {
		switch runtime.GOOS {
		case "windows":
			executable, err := os.Executable()
			if err != nil {
				return nil, err
			}
			path = filepath.Join(filepath.Dir(executable), "libass-9.dll")
		case "darwin":
			path = "libass.dylib"
		default:
			path = "libass.so.9"
		}
	}
	if runtime.GOOS == "windows" || strings.ContainsAny(path, `/\`) {
		var err error
		path, err = filepath.Abs(path)
		if err != nil {
			return nil, err
		}
	}
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	var message [1024]C.char
	handle := C.subtrim_open(cpath, &message[0], C.size_t(len(message)))
	if handle == nil {
		return nil, errors.New(C.GoString(&message[0]))
	}
	r := &Renderer{handle: handle, config: config, font: font}
	if err := r.load(data, false); err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}

func (r *Renderer) Close() {
	if r.handle != nil {
		C.subtrim_close(r.handle)
		r.handle = nil
	}
}

func (r *Renderer) load(data []byte, reuse bool) error {
	r.Events = nil
	r.boundaries, r.lastFrame = nil, nil
	r.frameTime, r.frameEnd = 0, 0
	if bytes.HasPrefix(data, []byte{0xff, 0xfe}) || bytes.HasPrefix(data, []byte{0xfe, 0xff}) {
		if len(data)%2 != 0 {
			return errors.New("invalid UTF-16 input")
		}
		var order binary.ByteOrder = binary.LittleEndian
		if data[0] == 0xfe {
			order = binary.BigEndian
		}
		units := make([]uint16, (len(data)-2)/2)
		for i := range units {
			units[i] = order.Uint16(data[2+i*2:])
		}
		for i := 0; i < len(units); i++ {
			if units[i] >= 0xd800 && units[i] <= 0xdbff {
				if i+1 == len(units) || units[i+1] < 0xdc00 || units[i+1] > 0xdfff {
					return errors.New("invalid UTF-16 surrogate")
				}
				i++
			} else if units[i] >= 0xdc00 && units[i] <= 0xdfff {
				return errors.New("invalid UTF-16 surrogate")
			}
		}
		data = []byte(string(utf16.Decode(units)))
	}
	fontFree := true
	for rest := data; ; {
		i := bytes.IndexByte(rest, '[')
		if i < 0 || len(rest)-i < len("[fonts]") {
			break
		}
		if bytes.EqualFold(rest[i:i+len("[fonts]")], []byte("[fonts]")) {
			fontFree = false
			break
		}
		rest = rest[i+1:]
	}
	reuse = reuse && r.fontFree && fontFree
	r.fontFree = fontFree
	if !reuse {
		r.ready = false
	}
	buffer := C.malloc(C.size_t(len(data) + 1))
	defer C.free(buffer)
	storage := unsafe.Slice((*byte)(buffer), len(data)+1)
	copy(storage, data)
	storage[len(data)] = 0
	fonts := C.CString(r.config.Fonts)
	defer C.free(unsafe.Pointer(fonts))
	keep := C.int(0)
	if reuse {
		keep = 1
	}
	track := C.subtrim_load(r.handle, (*C.char)(buffer), C.size_t(len(data)), fonts, keep)
	if track == nil {
		return errors.New("libass could not parse this document")
	}
	for _, event := range unsafe.Slice(track.events, int(track.n_events)) {
		r.Events = append(r.Events, Event{int64(event.Start), int64(event.Duration), C.GoString(event.Text), C.GoString(event.Effect)})
	}
	changes := map[int64]int{}
	for _, e := range r.Events {
		if e.Duration <= 0 {
			continue
		}
		end := e.Start + e.Duration
		if end <= e.Start {
			return nil
		}
		dynamic := 0
		// Automation labels are not time-dependent render effects. Keep all
		// possible legacy scroll spellings conservative, including invalid tails.
		effect := strings.ToLower(strings.TrimSpace(e.Effect))
		if strings.HasPrefix(effect, "banner") || strings.HasPrefix(effect, "scroll") || timeDependent.MatchString(e.Text) {
			dynamic = 1
		}
		changes[e.Start] += dynamic
		changes[end] -= dynamic
	}
	for t, delta := range changes {
		r.boundaries = append(r.boundaries, renderBoundary{t, delta})
	}
	slices.SortFunc(r.boundaries, func(a, b renderBoundary) int { return cmp.Compare(a.time, b.time) })
	active := 0
	for i := range r.boundaries {
		active += r.boundaries[i].dynamic
		r.boundaries[i].dynamic = active
	}
	return nil
}

// Reload starts fresh collision history, retaining font resources only when
// neither document embeds fonts. Callers scope reuse to one optimization.
func (r *Renderer) Reload(data []byte) error { return r.load(data, true) }

// image borrows native pixels until the next render on its owning renderer.
type image struct {
	w, h, stride, x, y int
	kind               int
	color              uint32
	bitmap             []byte
}

func (r *Renderer) frame(time int64, images []image) ([]image, bool, error) {
	if !r.ready {
		font := C.CString(r.font)
		defer C.free(unsafe.Pointer(font))
		w, h := r.config.StorageWidth, r.config.StorageHeight
		if w == 0 {
			w, h = r.config.Width, r.config.Height
		}
		if C.subtrim_configure(r.handle, C.int(r.config.Width), C.int(r.config.Height), C.int(w), C.int(h), font) == 0 {
			return nil, false, errors.New("libass could not initialize renderer")
		}
		r.ready = true
	}
	var changed C.int
	first := r.lastFrame
	if time < r.frameTime || time >= r.frameEnd {
		first = C.subtrim_frame(r.handle, C.longlong(time), &changed)
		r.lastFrame, r.frameTime, r.frameEnd = first, time, time
		i, found := slices.BinarySearchFunc(r.boundaries, time, func(b renderBoundary, t int64) int { return cmp.Compare(b.time, t) })
		if !found {
			i--
		}
		// All active events are time-independent until the next boundary.
		// Preserve the native image list and collision history in that interval.
		if i >= 0 && i+1 < len(r.boundaries) && r.boundaries[i].dynamic == 0 {
			r.frameEnd = r.boundaries[i+1].time
		}
	}
	r.frameTime = time
	images = images[:0]
	for p := first; p != nil; p = p.next {
		w, h, stride := int(p.w), int(p.h), int(p.stride)
		if w < 0 || h < 0 || stride < w || w > r.config.Width || h > r.config.Height {
			return nil, false, errors.New("invalid libass bitmap dimensions")
		}
		if w == 0 || h == 0 {
			continue
		}
		if p.bitmap == nil || (h-1) > (int(^uint(0)>>1)-w)/stride {
			return nil, false, errors.New("invalid libass bitmap buffer")
		}
		// the last row need not have stride bytes; never read its padding.
		pixels := unsafe.Slice((*byte)(unsafe.Pointer(p.bitmap)), (h-1)*stride+w)
		images = append(images, image{w, h, stride, int(p.dst_x), int(p.dst_y), int(p._type), uint32(p.color), pixels})
	}
	return images, changed != 0, nil
}

type Metrics struct {
	MaxChannelError float64 `json:"max_channel_error"`
	ActiveMeanError float64 `json:"active_mean_error"`
	ActivePixels    int     `json:"active_pixels"`
}

type Comparison struct {
	MeetsTolerance          bool    `json:"meets_tolerance"`
	CheckedTimestamps       int     `json:"checked_timestamps"`
	ActivePixelObservations int64   `json:"active_pixel_observations"`
	MaxChannelError         float64 `json:"max_channel_error"`
	MaxActiveMeanError      float64 `json:"max_active_mean_error"`
	FirstExceededMS         *int64  `json:"first_exceeded_ms"`
	LastExceededMS          *int64  `json:"last_exceeded_ms"`
	FailedTimestamps        int     `json:"failed_timestamps"`
}

// Compare advances both whole tracks in the supplied order, including reused frames.
// It stops on the first tolerance failure. Sampling coverage belongs to the caller.
func Compare(ctx context.Context, before, after *Renderer, times []int64, maxError, maxMean float64, cache *FrameCache) (Comparison, error) {
	return compare(ctx, before, after, times, maxError, maxMean, false, nil, cache)
}

// CompareAll checks every supplied timestamp, accumulating failures and global maxima.
// An optional failed callback receives each failed timestamp and its metrics.
func CompareAll(ctx context.Context, before, after *Renderer, times []int64, maxError, maxMean float64, failed func(int64, Metrics) error) (Comparison, error) {
	return compare(ctx, before, after, times, maxError, maxMean, true, failed, nil)
}

func compare(ctx context.Context, before, after *Renderer, times []int64, maxError, maxMean float64, complete bool, failed func(int64, Metrics) error, cache *FrameCache) (result Comparison, err error) {
	result = Comparison{MeetsTolerance: true}
	if before == after || before.handle == nil || after.handle == nil || before.config != after.config {
		return result, errors.New("comparison requires distinct open renderers with matching configurations")
	}
	if len(times) == 0 || maxError < 0 || maxMean < 0 || math.IsNaN(maxError) || math.IsNaN(maxMean) || math.IsInf(maxError, 0) || math.IsInf(maxMean, 0) {
		return result, errors.New("invalid comparison timestamps or tolerance")
	}
	var frames [2][]image
	var changed [2]bool
	var metric Metrics
	var canvas []float64
	var mask []byte
	// Separate renderers may advance concurrently, but each track must retain
	// its timestamp order. Bound read-ahead and join before reloading either track.
	type renderedFrame struct {
		images  []image
		changed bool
		err     error
	}
	var streams [2]chan renderedFrame
	if len(times) > 256 {
		workerCtx, cancel := context.WithCancel(ctx)
		done := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
		defer func() { cancel(); <-done[0]; <-done[1] }()
		for side, renderer := range []*Renderer{before, after} {
			stream := make(chan renderedFrame, 8)
			streams[side] = stream
			var activeCache *FrameCache
			if side == 0 && cache != nil {
				activeCache = cache
			}
			go func() {
				defer close(done[side])
				var images, previous []image
				for _, t := range times {
					if workerCtx.Err() != nil {
						return
					}
					var changed bool
					var err error
					if activeCache != nil {
						images, changed, err = activeCache.frame(workerCtx, t, images)
					} else {
						images, changed, err = renderer.frame(t, images)
					}
					var owned []image
					if err == nil {
						if activeCache != nil && activeCache.index <= len(activeCache.frames) {
							owned = activeCache.frames[activeCache.index-1].images
						} else if !changed && previous != nil {
							owned = previous
						} else {
							owned, _ = copyImages(images, previous, math.MaxInt)
						}
						previous = owned
					}
					select {
					case stream <- renderedFrame{owned, changed, err}:
					case <-workerCtx.Done():
						return
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}
	for index, time := range times {
		for side, renderer := range []*Renderer{before, after} {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			var err error
			if streams[side] != nil {
				select {
				case f := <-streams[side]:
					frames[side], changed[side], err = append(frames[side][:0], f.images...), f.changed, f.err
				case <-ctx.Done():
					return result, ctx.Err()
				}
			} else if side == 0 && cache != nil {
				frames[side], changed[side], err = cache.frame(ctx, time, frames[side])
			} else {
				frames[side], changed[side], err = renderer.frame(time, frames[side])
			}
			if err != nil {
				return result, fmt.Errorf("frame %d: %w", time, err)
			}
		}
		if index == 0 || changed[0] || changed[1] {
			metric, canvas, mask = measure(frames, before.config.Width, before.config.Height, canvas, mask)
		}
		result.CheckedTimestamps++
		result.ActivePixelObservations += int64(metric.ActivePixels)
		result.MaxChannelError = max(result.MaxChannelError, metric.MaxChannelError)
		result.MaxActiveMeanError = max(result.MaxActiveMeanError, metric.ActiveMeanError)
		if metric.MaxChannelError > maxError || metric.ActiveMeanError > maxMean {
			result.MeetsTolerance = false
			result.FailedTimestamps++
			if result.FirstExceededMS == nil {
				result.FirstExceededMS = &time
			}
			result.LastExceededMS = &time
			if failed != nil {
				if err := failed(time, metric); err != nil {
					return result, err
				}
			}
			if !complete {
				break
			}
		}
	}
	return result, nil
}

// CompareExact compares ordered native records without compositing. Only records
// with provably zero contribution are omitted, matching structural search policy.
func CompareExact(ctx context.Context, before, after *Renderer, times []int64, cache *FrameCache) (Comparison, error) {
	result := Comparison{MeetsTolerance: true}
	if before == after || before.handle == nil || after.handle == nil || before.config != after.config || len(times) == 0 {
		return result, errors.New("exact comparison requires distinct matching renderers and timestamps")
	}
	var frames [2][]image
	for index, t := range times {
		var changed [2]bool
		for side, r := range []*Renderer{before, after} {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			var err error
			if side == 0 && cache != nil {
				frames[side], changed[side], err = cache.frame(ctx, t, frames[side])
			} else {
				frames[side], changed[side], err = r.frame(t, frames[side])
			}
			if err != nil {
				return result, err
			}
		}
		result.CheckedTimestamps++
		if index > 0 && !changed[0] && !changed[1] {
			continue
		}
		for side, r := range []*Renderer{before, after} {
			visible := frames[side][:0]
			for _, im := range frames[side] {
				if im.color&255 == 255 || im.x >= r.config.Width || im.y >= r.config.Height || im.x+im.w <= 0 || im.y+im.h <= 0 {
					continue
				}
				nonzero := false
				for y := 0; y < im.h && !nonzero; y++ {
					for _, v := range im.bitmap[y*im.stride : y*im.stride+im.w] {
						if v != 0 {
							nonzero = true
							break
						}
					}
				}
				if nonzero {
					visible = append(visible, im)
				}
			}
			frames[side] = visible
		}
		same := len(frames[0]) == len(frames[1])
		for i := 0; same && i < len(frames[0]); i++ {
			a, b := frames[0][i], frames[1][i]
			same = a.w == b.w && a.h == b.h && a.x == b.x && a.y == b.y && a.color == b.color && a.kind == b.kind
			for y := 0; same && y < a.h; y++ {
				same = bytes.Equal(a.bitmap[y*a.stride:y*a.stride+a.w], b.bitmap[y*b.stride:y*b.stride+b.w])
			}
		}
		if !same {
			result.MeetsTolerance = false
			result.FirstExceededMS = &t
			result.LastExceededMS = &t
			result.FailedTimestamps = 1
			break
		}
	}
	return result, nil
}

func measure(frames [2][]image, width, height int, canvas []float64, mask []byte) (Metrics, []float64, []byte) {
	x0, y0, x1, y1 := width, height, 0, 0
	for side := range frames {
		visible := frames[side][:0]
		for _, im := range frames[side] {
			if im.color&255 == 255 || im.x >= width || im.y >= height || im.x+im.w <= 0 || im.y+im.h <= 0 {
				continue
			}
			visible = append(visible, im)
			x0 = min(x0, max(0, im.x))
			y0 = min(y0, max(0, im.y))
			x1 = max(x1, min(width, im.x+im.w))
			y1 = max(y1, min(height, im.y+im.h))
		}
		// the caller's slice length still counts transparent/offscreen images.
		frames[side] = visible
	}
	if x1 <= x0 || y1 <= y0 {
		return Metrics{}, canvas, mask
	}
	same := len(frames[0]) == len(frames[1])
	for i := 0; same && i < len(frames[0]); i++ {
		a, b := frames[0][i], frames[1][i]
		same = a.w == b.w && a.h == b.h && a.x == b.x && a.y == b.y && a.color == b.color
		for y := 0; same && y < a.h; y++ {
			same = bytes.Equal(a.bitmap[y*a.stride:y*a.stride+a.w], b.bitmap[y*b.stride:y*b.stride+b.w])
		}
	}
	rw, pixels := x1-x0, (x1-x0)*(y1-y0)
	if same {
		if cap(mask) < pixels {
			mask = make([]byte, pixels)
		} else {
			mask = mask[:pixels]
			clear(mask)
		}
	} else {
		if cap(canvas) < pixels*8 {
			canvas = make([]float64, pixels*8)
		} else {
			canvas = canvas[:pixels*8]
			clear(canvas)
		}
	}
	var result Metrics
	for side, images := range frames {
		if same && side == 1 {
			break
		}
		for _, im := range images {
			factor := float64(255-(im.color&255)) / (255.0 * 255.0)
			rgb := [3]float64{float64(im.color>>24) / 255, float64((im.color>>16)&255) / 255, float64((im.color>>8)&255) / 255}
			for y := max(0, im.y); y < min(height, im.y+im.h); y++ {
				left, right := max(0, im.x), min(width, im.x+im.w)
				dst := (y-y0)*rw + left - x0
				row := im.bitmap[(y-im.y)*im.stride+left-im.x : (y-im.y)*im.stride+right-im.x]
				for x, coverage := range row {
					if coverage == 0 {
						continue
					}
					if same {
						if mask[dst+x] == 0 {
							mask[dst+x] = 1
							result.ActivePixels++
						}
						continue
					}
					a := float64(coverage) * factor
					inverse := 1 - a
					d := canvas[(side*pixels+dst+x)*4:][:4]
					// explicit conversions prohibit fused operations, matching the C oracle.
					for k := 0; k < 3; k++ {
						d[k] = float64(d[k]*inverse) + float64(a*rgb[k])
					}
					d[3] = float64(d[3]*inverse) + a
				}
			}
		}
	}
	if same {
		return result, canvas, mask
	}
	errorSum, correction := 0.0, 0.0
	for i := 0; i < pixels; i++ {
		a, b := canvas[i*4:][:4], canvas[(pixels+i)*4:][:4]
		if a[3] <= 0 && b[3] <= 0 {
			continue
		}
		result.ActivePixels++
		da := a[3] - b[3]
		sum := math.Abs(da)
		result.MaxChannelError = max(result.MaxChannelError, sum)
		for k := 0; k < 3; k++ {
			delta := a[k] - b[k]
			raw := math.Abs(delta)
			difference := max(raw, math.Abs(delta-da))
			result.MaxChannelError = max(result.MaxChannelError, difference)
			sum += difference
		}
		adjusted := sum - correction
		next := errorSum + adjusted
		correction = (next - errorSum) - adjusted
		errorSum = next
	}
	errorSum *= 255
	result.MaxChannelError *= 255
	if result.ActivePixels > 0 {
		result.ActiveMeanError = errorSum / (4 * float64(result.ActivePixels))
	}
	return result, canvas, mask
}

// copyImages owns borrowed native pixels, sharing unchanged bitmaps with the
// previous frame. Check the cache budget before allocating any pixel buffers.
func copyImages(images, previous []image, limit int) ([]image, int) {
	size := 32 + 64*len(images)
	if size > limit {
		return nil, 0
	}
	owned := append([]image(nil), images...)
	for i, im := range images {
		owned[i].bitmap = nil
		if i < len(previous) && previous[i].w == im.w && previous[i].h == im.h {
			old := previous[i]
			equal := true
			for y := 0; equal && y < im.h; y++ {
				equal = bytes.Equal(old.bitmap[y*old.stride:y*old.stride+im.w], im.bitmap[y*im.stride:y*im.stride+im.w])
			}
			if equal {
				owned[i].bitmap = old.bitmap
			}
		}
		if owned[i].bitmap == nil {
			size += im.w * im.h
		}
	}
	if size > limit {
		return nil, 0
	}
	for i, im := range images {
		if owned[i].bitmap == nil {
			owned[i].bitmap = make([]byte, im.w*im.h)
			for y := 0; y < im.h; y++ {
				copy(owned[i].bitmap[y*im.w:], im.bitmap[y*im.stride:y*im.stride+im.w])
			}
		}
		owned[i].stride = im.w
	}
	return owned, size
}

type cachedFrame struct {
	time    int64
	images  []image
	changed bool
	bytes   int
}

// FrameCache is a bounded tape for one immutable document on one renderer.
// A matching timestamp prefix is the only reusable collision history.
type FrameCache struct {
	renderer         *Renderer
	data             []byte
	frames           []cachedFrame
	index, bytes     int
	replaying, reset bool
}

func NewFrameCache(renderer *Renderer, data []byte) *FrameCache {
	return &FrameCache{renderer: renderer, data: data}
}

// Begin starts a comparison. reset is required if this renderer has rendered
// anything since loading the document, including a different cache's preflight.
func (c *FrameCache) Begin(reset bool) { c.index = 0; c.replaying = false; c.reset = reset }

func (c *FrameCache) frame(ctx context.Context, t int64, images []image) ([]image, bool, error) {
	if !c.replaying && c.index < len(c.frames) && c.frames[c.index].time == t {
		f := c.frames[c.index]
		c.index++
		return append(images[:0], f.images...), f.changed, nil
	}
	if !c.replaying {
		if c.reset {
			if err := c.renderer.Reload(c.data); err != nil {
				return nil, false, err
			}
		}
		for _, f := range c.frames[:c.index] {
			if err := ctx.Err(); err != nil {
				return nil, false, err
			}
			var err error
			images, _, err = c.renderer.frame(f.time, images)
			if err != nil {
				return nil, false, err
			}
		}
		clear(c.frames[c.index:])
		c.frames = c.frames[:c.index]
		c.bytes = 0
		for _, f := range c.frames {
			c.bytes += f.bytes
		}
		c.replaying = true
	}
	images, changed, err := c.renderer.frame(t, images)
	if err != nil {
		return nil, false, err
	}
	if c.index == len(c.frames) {
		var previous []image
		if c.index > 0 {
			previous = c.frames[c.index-1].images
		}
		owned, size := previous, 32
		if changed || c.index == 0 {
			owned, size = copyImages(images, previous, 32<<20-c.bytes)
		}
		if size > 0 && size <= 32<<20-c.bytes {
			c.frames = append(c.frames, cachedFrame{t, owned, changed, size})
			c.bytes += size
		}
	}
	c.index++
	return images, changed, nil
}
