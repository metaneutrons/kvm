package kvm

import (
	"bytes"
	"errors"
	"image"
	"image/jpeg"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/jetkvm/kvm/internal/native"
)

// The relative mouse moves the pointer by counts, which the host scales and
// may accelerate. calibrate measures how far the host moves the pointer per
// count: it moves the pointer from the corner to one point, and again from the
// corner to a second one, and finds it in the video by the pixels that differ
// in only one of three frames. The distance
// between the two finds gives the scale; the pointer's shape and the offset
// of its hotspot cancel out.

const (
	// calibFirst and calibSecond are the calibration points in counts from
	// the top-left corner, on both axes.
	calibFirst  = 100
	calibSecond = 300
	// calibSettle lets the host draw the pointer before a frame is taken.
	calibSettle = 150 * time.Millisecond
	// calibMinPixels is the smallest changed area taken for the pointer.
	calibMinPixels = 12
	// calibMaxOffset bounds the distance in video pixels between the
	// pointer's hotspot and the top-left of its image.
	calibMaxOffset = 40
	// calibCandidates is how many of the largest changed areas per frame are
	// tried as the pointer.
	calibCandidates = 5
	calibValidity   = 30 * time.Minute
	// A scale outside these bounds, in video pixels per count, is taken as a
	// wrong find.
	calibMinScale = 0.05
	calibMaxScale = 20
)

// relScale is the measured scale of the relative mouse for one video size.
type relScale struct {
	videoW, videoH int
	x, y           float64 // video pixels per count
	at             time.Time
}

var (
	relScaleMu     sync.Mutex
	relScaleCached *relScale
)

// cachedRelScale returns the scale measured for this video size, if recent.
func cachedRelScale(videoW, videoH int) (float64, float64, bool) {
	relScaleMu.Lock()
	defer relScaleMu.Unlock()
	c := relScaleCached
	if c == nil || c.videoW != videoW || c.videoH != videoH || time.Since(c.at) > calibValidity {
		return 0, 0, false
	}
	return c.x, c.y, true
}

func storeRelScale(videoW, videoH int, x, y float64) {
	relScaleMu.Lock()
	defer relScaleMu.Unlock()
	relScaleCached = &relScale{videoW: videoW, videoH: videoH, x: x, y: y, at: time.Now()}
}

// calibrate measures the relative mouse's scale with the pointer homed in the
// corner and leaves it at the second calibration point. It returns ok=false if
// the pointer could not be found in the video.
func (p *mcpPointer) calibrate(videoW, videoH int) (scaleX, scaleY float64, ok bool, err error) {
	// The calibration points lie in the top-left quarter for scales up to
	// about 3 at 1080p; a larger scale is not found and falls back to 1.
	regionW, regionH := (videoW/2)&^1, (videoH/2)&^1
	frame := func() (*image.YCbCr, error) {
		time.Sleep(calibSettle)
		data, err := takeVideoSnapshot(native.SnapshotOptions{Quality: 90, CropWidth: regionW, CropHeight: regionH})
		if err != nil {
			return nil, err
		}
		img, err := jpeg.Decode(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		ycc, isYCC := img.(*image.YCbCr)
		if !isYCC {
			return nil, errors.New("unexpected snapshot format")
		}
		return ycc, nil
	}

	corner, err := frame()
	if err != nil {
		return 0, 0, false, err
	}
	if err := p.stepTo(calibFirst, calibFirst, 0); err != nil {
		return 0, 0, false, err
	}
	first, err := frame()
	if err != nil {
		return 0, 0, false, err
	}
	// The host scales a movement that starts from rest differently, so both
	// points are reached in one movement from the corner, like a real move.
	if err := p.toCorner(); err != nil {
		return 0, 0, false, err
	}
	if err := p.stepTo(calibSecond, calibSecond, 0); err != nil {
		return 0, 0, false, err
	}
	second, err := frame()
	if err != nil {
		return 0, 0, false, err
	}

	atFirst := changedAreas(first, corner, second)
	atSecond := changedAreas(second, corner, first)
	scaleX, scaleY, ok = pointerScale(atFirst, atSecond, calibFirst, calibSecond)
	apiLogger.Info().Bool("found", ok).Float64("scale_x", scaleX).Float64("scale_y", scaleY).
		Interface("at_first", atFirst).Interface("at_second", atSecond).
		Msg("calibrated the relative mouse")
	return scaleX, scaleY, ok, nil
}

// pointerScale picks the pair of changed areas that fits a pointer moved from
// count a to count b with a small hotspot offset, and returns its scale.
func pointerScale(atA, atB []image.Point, a, b int) (float64, float64, bool) {
	best, found := math.Inf(1), false
	var scaleX, scaleY float64
	for _, pa := range atA {
		for _, pb := range atB {
			sx := float64(pb.X-pa.X) / float64(b-a)
			sy := float64(pb.Y-pa.Y) / float64(b-a)
			if sx < calibMinScale || sx > calibMaxScale || sy < calibMinScale || sy > calibMaxScale {
				continue
			}
			ox := float64(pa.X) - sx*float64(a)
			oy := float64(pa.Y) - sy*float64(a)
			offset := math.Hypot(ox, oy)
			if math.Abs(ox) > calibMaxOffset || math.Abs(oy) > calibMaxOffset || offset >= best {
				continue
			}
			best, found, scaleX, scaleY = offset, true, sx, sy
		}
	}
	return scaleX, scaleY, found
}

// changedAreas returns the top-left corners of the largest connected areas of
// pixels whose brightness in target differs from every other frame by more
// than mcpPixelNoise: what is drawn only in target, such as the pointer.
func changedAreas(target *image.YCbCr, others ...*image.YCbCr) []image.Point {
	r := target.Rect
	for _, o := range others {
		if o.Rect != r {
			return nil
		}
	}
	w, h := r.Dx(), r.Dy()
	changed := make([]bool, w*h)
	for y := range h {
		for x := range w {
			v := int(target.Y[target.YOffset(r.Min.X+x, r.Min.Y+y)])
			only := true
			for _, o := range others {
				d := v - int(o.Y[o.YOffset(r.Min.X+x, r.Min.Y+y)])
				if d <= mcpPixelNoise && d >= -mcpPixelNoise {
					only = false
					break
				}
			}
			changed[y*w+x] = only
		}
	}

	type area struct {
		min  image.Point
		size int
	}
	var areas []area
	queue := make([]int, 0, 256)
	for start, isChanged := range changed {
		if !isChanged {
			continue
		}
		changed[start] = false
		queue = append(queue[:0], start)
		a := area{min: image.Point{X: w, Y: h}}
		for len(queue) > 0 {
			i := queue[len(queue)-1]
			queue = queue[:len(queue)-1]
			x, y := i%w, i/w
			a.size++
			a.min.X = min(a.min.X, x)
			a.min.Y = min(a.min.Y, y)
			// The pointer's outline may have gaps of a pixel, so neighbours
			// two pixels away count as connected.
			for dy := -2; dy <= 2; dy++ {
				for dx := -2; dx <= 2; dx++ {
					nx, ny := x+dx, y+dy
					if nx < 0 || ny < 0 || nx >= w || ny >= h || !changed[ny*w+nx] {
						continue
					}
					changed[ny*w+nx] = false
					queue = append(queue, ny*w+nx)
				}
			}
		}
		if a.size >= calibMinPixels {
			areas = append(areas, a)
		}
	}
	sort.Slice(areas, func(i, j int) bool { return areas[i].size > areas[j].size })
	if len(areas) > calibCandidates {
		areas = areas[:calibCandidates]
	}
	points := make([]image.Point, len(areas))
	for i, a := range areas {
		points[i] = a.min
	}
	return points
}
