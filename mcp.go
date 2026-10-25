package kvm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jetkvm/kvm/internal/hidrpc"
	"github.com/jetkvm/kvm/internal/keyboard"
	"github.com/jetkvm/kvm/internal/native"
	"github.com/jetkvm/kvm/internal/usbgadget"
)

// /api/mcp is a Model Context Protocol server that lets an AI agent see the
// host's screen and use its keyboard and mouse. It runs in stateless mode, so
// every POST is a complete exchange, and shares the API token and the tool
// set's limits with /api/rpc.

const (
	mcpMaxTextLength = 4096
	// mcpKeyDelay matches the paste dialog's default delay between keys.
	mcpKeyDelay    = 20 * time.Millisecond
	mcpClickHold   = 50 * time.Millisecond
	mcpDoubleClick = 80 * time.Millisecond
	mcpDragSteps   = 10
	// mcpDragNudge is about one pixel of a 1080p screen in absolute units.
	mcpDragNudge = 16
	// Without the absolute mouse, the pointer is pushed into the top-left
	// corner by mcpRelHome reports of -127 and then moved in steps of at most
	// mcpRelStep counts, which keeps the host's pointer acceleration low.
	mcpRelHome     = 40
	mcpRelStep     = 8
	mcpRelInterval = 4 * time.Millisecond
	mcpMaxScroll   = 50
	// mcpMaxHold allows holding a key through a boot, e.g. Option on a Mac.
	mcpMaxHold = 30 * time.Second
	// mcpMaxWait bounds the wait tool.
	mcpMaxWait = 60 * time.Second
	// mcpSettleWait bounds the wait for the screen to settle before the
	// screenshot an input tool returns on request.
	mcpSettleWait = 3 * time.Second
	absMouseMax   = 32767
	// mcpDefaultScreenWidth is the widest screen the agent sees unless the
	// client asks for another width. Larger video is scaled down to it, which
	// keeps screenshots small and below the sizes at which clients scale
	// images themselves; the mouse tools use the same coordinates, so they
	// match what the model sees.
	mcpDefaultScreenWidth = 1280
	mcpMinScreenWidth     = 320
	mcpMaxScreenWidth     = 7680
)

// mcpScreen is the widest screen a client's agent sees; 0 shows the video at
// its own resolution. A client picks it once, in the endpoint URL, so every
// call of a session uses the same coordinates.
type mcpScreen int

// parseMCPScreen reads the screen_width query parameter: empty for the
// default, "native" for the video's own resolution, or a width in pixels.
func parseMCPScreen(raw string) (mcpScreen, error) {
	switch raw {
	case "":
		return mcpDefaultScreenWidth, nil
	case "native":
		return 0, nil
	}
	width, err := strconv.Atoi(raw)
	if err != nil || width < mcpMinScreenWidth || width > mcpMaxScreenWidth {
		return 0, fmt.Errorf("screen_width must be native or a width from %d to %d", mcpMinScreenWidth, mcpMaxScreenWidth)
	}
	return mcpScreen(width), nil
}

func (s mcpScreen) instructions(readOnly bool) string {
	size := fmt.Sprintf("scaled to at most %d pixels wide", int(s))
	if s == 0 {
		size = "at the video's own resolution"
	}
	return `This server controls the computer attached to a JetKVM. It sees the computer's HDMI output and acts as its USB keyboard and mouse.
Take a screenshot first. It shows the whole screen, ` + size + `, and states the screen size. All mouse tools take x,y in the pixels of that full screenshot, with 0,0 at the top left. Fractions such as 412.5 are allowed for precise positions.
To read small text or check details, take a screenshot with a region: it returns that part of the screen enlarged up to the real resolution. Coordinates for the mouse tools stay those of the full screenshot; the response explains how to convert a point in the zoomed image.
type_text types characters through a keyboard layout, which must match the layout the host uses; for a Mac pick the macOS variant, such as de-DE-macos (get_state lists them). press_keys sends key combinations by key name.
Input tools take screenshot: true to return a screenshot once the screen has settled after the action, which saves a separate call. The wait tool waits for the screen to change or settle, for example while an app starts.
If the screenshot reports no signal, the display may be asleep: press a key such as ShiftLeft or call wake_host, then take a new screenshot.` + viewOnlyNote(readOnly)
}

func viewOnlyNote(readOnly bool) string {
	if !readOnly {
		return ""
	}
	return "\nThis connection uses a view-only token: only screenshot and get_state are available, and no input can be sent."
}

// mcpServerKey selects a server: the tool descriptions and coordinates depend
// on the screen width, and a view-only token gets no input tools.
type mcpServerKey struct {
	screen   mcpScreen
	readOnly bool
}

var mcpServers sync.Map

var mcpHandler = sync.OnceValue(func() http.Handler {
	return mcp.NewStreamableHTTPHandler(
		func(r *http.Request) *mcp.Server {
			screen, err := parseMCPScreen(r.URL.Query().Get("screen_width"))
			if err != nil {
				return nil
			}
			key := mcpServerKey{screen: screen, readOnly: apiScope(r.Context()) == apiScopeView}
			if server, ok := mcpServers.Load(key); ok {
				return server.(*mcp.Server)
			}
			server, _ := mcpServers.LoadOrStore(key, newMCPServer(key.screen, key.readOnly))
			return server.(*mcp.Server)
		},
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true},
	)
})

func handleMCP(c *gin.Context) {
	if _, err := parseMCPScreen(c.Query("screen_width")); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	mcpHandler().ServeHTTP(c.Writer, c.Request)
}

var (
	mcpReadOnly = &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: boolPtr(false)}
	// Input reaches a computer whose state the server cannot see.
	mcpInput = &mcp.ToolAnnotations{DestructiveHint: boolPtr(true), OpenWorldHint: boolPtr(false)}
)

// mcpAfter is embedded in input tools' arguments to ask for a screenshot of
// the result.
type mcpAfter struct {
	Screenshot bool `json:"screenshot,omitempty" jsonschema:"return a screenshot once the screen has settled after the action"`
}

func (a mcpAfter) wantsScreenshot() bool { return a.Screenshot }

// trackMCP logs an input tool's call and tells the web UI that an agent is
// active before the tool runs. If asked, it adds a screenshot of the result.
func trackMCP[In, Out any](screen mcpScreen, name string, handler mcp.ToolHandlerFor[In, Out]) mcp.ToolHandlerFor[In, Out] {
	return func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		noteAgentAction("mcp", name, mcpActionDetail(in))
		result, out, err := handler(ctx, req, in)
		after, ok := any(in).(interface{ wantsScreenshot() bool })
		if err != nil || result == nil || !ok || !after.wantsScreenshot() {
			return result, out, err
		}
		waitForScreen(ctx, waitUntilStable, mcpSettleWait)
		shot, shotErr := screen.capture(0, nil)
		if shotErr != nil {
			result.Content = append(result.Content, &mcp.TextContent{Text: "No screenshot: " + shotErr.Error()})
		} else {
			result.Content = append(result.Content, shot...)
		}
		return result, out, err
	}
}

// mcpActionDetail describes a tool call for the log. Typed text is left out,
// as it may be a password.
func mcpActionDetail(in any) string {
	switch v := in.(type) {
	case mcpTypeTextInput:
		return fmt.Sprintf("%d characters, layout %q", len([]rune(v.Text)), v.Layout)
	case mcpJigglerInput:
		if v.Enabled == nil {
			return "report state"
		}
		return fmt.Sprintf("enabled=%t", *v.Enabled)
	case mcpEmpty:
		return ""
	}
	return fmt.Sprintf("%+v", in)
}

func newMCPServer(screen mcpScreen, readOnly bool) *mcp.Server {
	server := mcp.NewServer(
		&mcp.Implementation{Name: "jetkvm", Title: "JetKVM", Version: builtAppVersion},
		&mcp.ServerOptions{Instructions: screen.instructions(readOnly)},
	)

	fullScreen := fmt.Sprintf("scaled to at most %d pixels wide", int(screen))
	if screen == 0 {
		fullScreen = "at the video's own resolution"
	}
	mcp.AddTool(server, &mcp.Tool{
		Name: "screenshot",
		Description: "Capture the host's screen as a JPEG image. Without a region it shows the whole screen, " + fullScreen + "; " +
			"its pixels are the coordinates every mouse tool uses. With a region (in those same coordinates) it shows only that part, " +
			"enlarged up to the real video resolution, to read small text. The response states the sizes and how to map a point in a zoomed image back to screen coordinates.",
		Annotations: mcpReadOnly,
	}, screen.screenshot)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_state",
		Description: "Report the video signal, USB connection, keyboard layout and LEDs, virtual media and power state.",
		Annotations: mcpReadOnly,
	}, mcpGetState)
	mcp.AddTool(server, &mcp.Tool{
		Name: "wait",
		Description: "Wait before the next step: a number of seconds, or until the screen changes (until: change) " +
			"or stops changing (until: stable), at most 60 seconds. Use it while an app starts, a page loads or the host boots. " +
			"Live content such as a ticking clock or a CPU graph counts as a change, but does not keep the screen from being stable.",
		Annotations: mcpReadOnly,
	}, mcpWait)
	if readOnly {
		return server
	}
	mcp.AddTool(server, &mcp.Tool{
		Name:        "move_mouse",
		Description: "Move the mouse pointer to a screen position.",
		Annotations: mcpInput,
	}, trackMCP(screen, "move_mouse", screen.moveMouse))
	mcp.AddTool(server, &mcp.Tool{
		Name:        "click",
		Description: "Move the mouse pointer to a screen position and click a button there.",
		Annotations: mcpInput,
	}, trackMCP(screen, "click", screen.click))
	mcp.AddTool(server, &mcp.Tool{
		Name:        "drag",
		Description: "Press a mouse button at one screen position, move to another and release it there.",
		Annotations: mcpInput,
	}, trackMCP(screen, "drag", screen.drag))
	mcp.AddTool(server, &mcp.Tool{
		Name: "scroll",
		Description: "Move the mouse pointer to a screen position and turn the scroll wheel there. " +
			"Positive delta_y turns the wheel down, which scrolls down on Windows and Linux. macOS reverses it with natural scrolling, its default, " +
			"so check the direction with a screenshot.",
		Annotations: mcpInput,
	}, trackMCP(screen, "scroll", screen.scroll))
	mcp.AddTool(server, &mcp.Tool{
		Name: "press_keys",
		Description: "Press keys together and release them, for shortcuts and special keys. " +
			`Keys are named after the physical US key position, as in the browser's KeyboardEvent.code: "KeyA", "Digit1", "Enter", "Tab", "Escape", "Backspace", "ArrowUp", "F5", "ControlLeft", "ShiftLeft", "AltLeft", "AltRight", "MetaLeft". ` +
			`Ctrl, Shift, Alt, Cmd, Win and Esc are accepted as short forms. Example: ["ControlLeft", "KeyC"].`,
		Annotations: mcpInput,
	}, trackMCP(screen, "press_keys", mcpPressKeys))
	mcp.AddTool(server, &mcp.Tool{
		Name: "type_text",
		Description: "Type text on the host's keyboard. Characters are mapped through a keyboard layout, " +
			"which must match the layout the host uses: for a Mac use the macOS variant such as de-DE-macos or en-US-macos, for Windows and Linux the plain one such as de-DE. " +
			"The default is the layout set in the JetKVM. Newlines press Enter. Fails without typing anything if a character is not in the layout.",
		Annotations: mcpInput,
	}, trackMCP(screen, "type_text", mcpTypeText))
	mcp.AddTool(server, &mcp.Tool{
		Name:        "wake_host",
		Description: "Send a USB remote wakeup signal to wake the host from sleep.",
		Annotations: mcpInput,
	}, trackMCP(screen, "wake_host", mcpWakeHost))
	mcp.AddTool(server, &mcp.Tool{
		Name: "jiggler",
		Description: "Turn the JetKVM's mouse jiggler on or off. While it is on, it moves the mouse by a few pixels when no input arrived for a while, " +
			"which keeps the host from sleeping or locking its screen. The setting is saved on the JetKVM and stays until changed, also for people using the web UI. " +
			"Without enabled, it only reports the current state.",
		Annotations: mcpInput,
	}, trackMCP(screen, "jiggler", mcpJiggler))
	mcp.AddTool(server, &mcp.Tool{
		Name: "power",
		Description: "Use the power extension. With the ATX extension: power-short presses the power button, power-long holds it for five seconds to force off, reset presses reset. " +
			"With the DC extension: on and off switch the host's power supply.",
		Annotations: mcpInput,
	}, trackMCP(screen, "power", mcpPower))
	mcp.AddTool(server, &mcp.Tool{
		Name:        "mount_media",
		Description: "Attach an image to the host as a USB drive, either from a URL or from a file in the JetKVM's storage.",
		Annotations: mcpInput,
	}, trackMCP(screen, "mount_media", mcpMountMedia))
	mcp.AddTool(server, &mcp.Tool{
		Name:        "unmount_media",
		Description: "Detach the virtual USB drive from the host.",
		Annotations: mcpInput,
	}, trackMCP(screen, "unmount_media", mcpUnmountMedia))

	return server
}

type mcpEmpty struct{}

// mcpDone reports a completed action. An empty result is valid, but leaves
// some clients and models unsure whether anything happened.
func mcpDone(format string, args ...any) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{Content: []mcp.Content{
		&mcp.TextContent{Text: fmt.Sprintf(format, args...)},
	}}, nil, nil
}

type mcpRegion struct {
	X      int `json:"x" jsonschema:"left edge in screen coordinates"`
	Y      int `json:"y" jsonschema:"top edge in screen coordinates"`
	Width  int `json:"width" jsonschema:"width in screen coordinates"`
	Height int `json:"height" jsonschema:"height in screen coordinates"`
}

type mcpScreenshotInput struct {
	Quality int        `json:"quality,omitempty" jsonschema:"JPEG quality from 1 to 99, default 80"`
	Region  *mcpRegion `json:"region,omitempty" jsonschema:"part of the screen to zoom into, in screen coordinates; omit for the whole screen"`
}

// size returns the size of the screen the agent works with: the video size,
// scaled down to the screen width if it is wider.
func (s mcpScreen) size(videoWidth, videoHeight int) (int, int) {
	width := int(s)
	if width == 0 || videoWidth <= width {
		return videoWidth, videoHeight
	}
	return width, (videoHeight*width + videoWidth/2) / videoWidth
}

// snapshotOptions turns a screenshot request in screen coordinates into
// snapshot options in video pixels. A region is captured at the video's own
// resolution, but never wider than the screen.
func (s mcpScreen) snapshotOptions(quality int, region *mcpRegion, videoWidth, videoHeight int) (native.SnapshotOptions, error) {
	opts := native.SnapshotOptions{Quality: quality}
	screenWidth, screenHeight := s.size(videoWidth, videoHeight)
	if region == nil {
		if screenWidth != videoWidth {
			opts.Width, opts.Height = screenWidth, screenHeight
		}
		return opts, nil
	}

	r := *region
	if r.Width <= 0 || r.Height <= 0 || r.X < 0 || r.Y < 0 || r.X+r.Width > screenWidth || r.Y+r.Height > screenHeight {
		return opts, fmt.Errorf("region %d,%d %dx%d is not within the %dx%d screen", r.X, r.Y, r.Width, r.Height, screenWidth, screenHeight)
	}
	if r.Width < 16 || r.Height < 16 {
		return opts, errors.New("region must be at least 16x16")
	}
	toVideo := func(v, screen, video int) int { return v * video / screen }
	opts.CropX = toVideo(r.X, screenWidth, videoWidth)
	opts.CropY = toVideo(r.Y, screenHeight, videoHeight)
	opts.CropWidth = toVideo(r.Width, screenWidth, videoWidth)
	opts.CropHeight = toVideo(r.Height, screenHeight, videoHeight)
	if opts.CropWidth > screenWidth {
		opts.Width = screenWidth
		opts.Height = (opts.CropHeight*screenWidth + opts.CropWidth/2) / opts.CropWidth
	}
	return opts, nil
}

func (s mcpScreen) screenshot(_ context.Context, _ *mcp.CallToolRequest, in mcpScreenshotInput) (*mcp.CallToolResult, any, error) {
	noteAgentView("mcp", "screenshot")
	content, err := s.capture(in.Quality, in.Region)
	if err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{Content: content}, nil, nil
}

// capture takes a screenshot of the screen or a region of it and describes
// its coordinates for the model.
func (s mcpScreen) capture(quality int, region *mcpRegion) ([]mcp.Content, error) {
	if quality == 0 {
		quality = native.DefaultSnapshotQuality
	}
	if quality < 1 || quality > 99 {
		return nil, errors.New("quality must be between 1 and 99")
	}

	// The video size is only known while there is a signal. Without one the
	// first capture wakes the display, and the request is planned again.
	var image []byte
	for attempt := 0; ; attempt++ {
		videoWidth, videoHeight := lastVideoState.Width, lastVideoState.Height
		known := videoWidth > 0 && videoHeight > 0
		opts := native.SnapshotOptions{Quality: quality}
		if known {
			var err error
			if opts, err = s.snapshotOptions(quality, region, videoWidth, videoHeight); err != nil {
				return nil, err
			}
		}
		var err error
		image, err = takeVideoSnapshot(opts)
		var noSignal errNoVideoSignal
		switch {
		case errors.As(err, &noSignal):
			return nil, fmt.Errorf("no video signal from the host (%s); the display may be asleep, press a key or call wake_host", noSignal.reason)
		case err != nil:
			return nil, err
		}
		if known || attempt > 0 || lastVideoState.Width <= 0 {
			break
		}
	}

	cfg, err := jpeg.DecodeConfig(bytes.NewReader(image))
	if err != nil {
		return nil, fmt.Errorf("failed to read the snapshot: %w", err)
	}
	screenWidth, screenHeight := s.size(lastVideoState.Width, lastVideoState.Height)
	var text string
	if region == nil {
		text = fmt.Sprintf("Full screen, %dx%d. Mouse tools take x,y in these pixels; fractions are allowed.", screenWidth, screenHeight)
	} else {
		r := region
		text = fmt.Sprintf("Zoomed region x=%d y=%d width=%d height=%d of the %dx%d screen, shown at %dx%d (%.1f times). "+
			"Mouse tools still take full-screen coordinates: a point u,v in this image is at x = %d + u*%d/%d, y = %d + v*%d/%d.",
			r.X, r.Y, r.Width, r.Height, screenWidth, screenHeight, cfg.Width, cfg.Height,
			float64(cfg.Width)/float64(r.Width), r.X, r.Width, cfg.Width, r.Y, r.Height, cfg.Height)
	}
	return []mcp.Content{
		&mcp.ImageContent{Data: image, MIMEType: "image/jpeg"},
		&mcp.TextContent{Text: text},
	}, nil
}

const (
	waitUntilChange = "change"
	waitUntilStable = "stable"
	// The screen is compared on thumbnails by the share of pixels whose
	// brightness changed by more than mcpPixelNoise. Capture and encoding
	// shift unchanged pixels by up to about 15 levels, which this ignores;
	// beyond that, noise was measured at no more than 6 of 57600 pixels.
	// "Changed" needs about 12 pixels, a checkbox being ticked; "stable"
	// allows a small live element such as a clock or a CPU graph.
	mcpThumbWidth   = 320
	mcpPixelNoise   = 24
	mcpChangedShare = 0.0002
	mcpStableShare  = 0.002
	// mcpWaitPoll keeps a waiting agent from occupying the encoder and the
	// single CPU core: each poll encodes and decodes a thumbnail.
	mcpWaitPoll = 400 * time.Millisecond
)

func screenThumbnail() (image.Image, error) {
	data, err := takeVideoSnapshot(native.SnapshotOptions{Quality: 50, Width: mcpThumbWidth})
	if err != nil {
		return nil, err
	}
	return jpeg.Decode(bytes.NewReader(data))
}

// screenDifference returns the share of pixels whose brightness differs by
// more than mcpPixelNoise between two thumbnails, or 1 if they cannot be
// compared.
func screenDifference(a, b image.Image) float64 {
	ya, okA := a.(*image.YCbCr)
	yb, okB := b.(*image.YCbCr)
	if !okA || !okB || ya.Rect != yb.Rect || ya.Rect.Empty() {
		return 1
	}
	changed := 0
	for y := ya.Rect.Min.Y; y < ya.Rect.Max.Y; y++ {
		for x := ya.Rect.Min.X; x < ya.Rect.Max.X; x++ {
			d := int(ya.Y[ya.YOffset(x, y)]) - int(yb.Y[yb.YOffset(x, y)])
			if d > mcpPixelNoise || d < -mcpPixelNoise {
				changed++
			}
		}
	}
	return float64(changed) / float64(ya.Rect.Dx()*ya.Rect.Dy())
}

// waitForScreen waits until the screen changes or stops changing, at most
// for timeout. It reports whether that happened.
func waitForScreen(ctx context.Context, until string, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	previous, err := screenThumbnail()
	if err != nil {
		return false, err
	}
	reference := previous
	stableRounds := 0
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(mcpWaitPoll):
		}
		current, err := screenThumbnail()
		if err != nil {
			return false, err
		}
		switch until {
		case waitUntilChange:
			if screenDifference(reference, current) > mcpChangedShare {
				return true, nil
			}
		case waitUntilStable:
			// Two quiet intervals in a row, so a slow animation is not
			// mistaken for a settled screen.
			if screenDifference(previous, current) <= mcpStableShare {
				stableRounds++
				if stableRounds >= 2 {
					return true, nil
				}
			} else {
				stableRounds = 0
			}
		}
		previous = current
	}
	return false, nil
}

type mcpWaitInput struct {
	Seconds float64 `json:"seconds,omitempty" jsonschema:"how long to wait; with until, the most to wait; default 1, or 10 with until; at most 60"`
	Until   string  `json:"until,omitempty" jsonschema:"change: until the screen changes; stable: until it stops changing; empty: the full time"`
}

func mcpWait(ctx context.Context, _ *mcp.CallToolRequest, in mcpWaitInput) (*mcp.CallToolResult, any, error) {
	timeout := time.Second
	if in.Until != "" {
		timeout = 10 * time.Second
	}
	if in.Seconds > 0 {
		timeout = time.Duration(in.Seconds * float64(time.Second))
	}
	if timeout > mcpMaxWait {
		return nil, nil, fmt.Errorf("wait at most %s", mcpMaxWait)
	}
	start := time.Now()
	switch in.Until {
	case "":
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(timeout):
		}
		return mcpDone("Waited %.1f seconds.", timeout.Seconds())
	case waitUntilChange, waitUntilStable:
		done, err := waitForScreen(ctx, in.Until, timeout)
		if err != nil {
			return nil, nil, err
		}
		elapsed := time.Since(start).Seconds()
		switch {
		case done && in.Until == waitUntilChange:
			return mcpDone("The screen changed after %.1f seconds.", elapsed)
		case done:
			return mcpDone("The screen was stable after %.1f seconds.", elapsed)
		case in.Until == waitUntilChange:
			return mcpDone("The screen did not change within %.1f seconds.", elapsed)
		default:
			return mcpDone("The screen was still changing after %.1f seconds.", elapsed)
		}
	}
	return nil, nil, fmt.Errorf("unknown until %q, use change or stable", in.Until)
}

func requireHID() error {
	if gadget == nil || !usbReadyForHidReports() {
		return errors.New("the JetKVM is not connected to the host over USB")
	}
	return nil
}

// mcpPointer moves the host's pointer to screen positions. It uses the
// absolute mouse, or the relative mouse when the absolute one is disabled in
// the USB settings, as some BIOS setups need. The relative mouse has no
// position of its own: the pointer is pushed into the top-left corner first and
// moved from there by the scale calibrate measured for the host. Without a
// measurement it assumes one count per video pixel, and the tools say so.
type mcpPointer struct {
	w, h           int // screen size of the coordinates
	videoW, videoH int
	relative       bool
	// absolute mouse: the last position sent
	ax, ay int
	// relative mouse: counts per screen pixel, whether they were measured,
	// and the position in counts from the corner once homed
	gainX, gainY float64
	calibrated   bool
	homed        bool
	cx, cy       int
}

func (s mcpScreen) pointer() (*mcpPointer, error) {
	if err := requireHID(); err != nil {
		return nil, err
	}
	videoW, videoH := lastVideoState.Width, lastVideoState.Height
	w, h := s.size(videoW, videoH)
	if w <= 0 || h <= 0 {
		return nil, errors.New("the screen size is unknown because there is no video signal")
	}
	p := &mcpPointer{w: w, h: h, videoW: videoW, videoH: videoH}
	switch {
	case gadget.HasAbsoluteMouse():
	case gadget.HasRelativeMouse():
		p.relative = true
	default:
		return nil, errors.New("both mice are disabled in the JetKVM's USB settings")
	}
	return p, nil
}

// check rejects a position outside the screen before anything is sent.
func (p *mcpPointer) check(x, y float64) error {
	_, _, err := absPosition(x, y, p.w, p.h)
	return err
}

// moveTo moves the pointer to x,y with buttons held.
func (p *mcpPointer) moveTo(x, y float64, buttons uint8) error {
	if !p.relative {
		ax, ay, err := absPosition(x, y, p.w, p.h)
		if err != nil {
			return err
		}
		p.ax, p.ay = ax, ay
		return rpcAbsMouseReport(ax, ay, buttons)
	}
	if err := p.check(x, y); err != nil {
		return err
	}
	if !p.homed {
		if err := p.home(); err != nil {
			return err
		}
	}
	return p.stepTo(int(math.Round(x*p.gainX)), int(math.Round(y*p.gainY)), buttons)
}

// home pushes the relative pointer into the top-left corner and sets the
// scale, measuring it first unless a recent measurement exists.
func (p *mcpPointer) home() error {
	if err := p.toCorner(); err != nil {
		return err
	}

	scaleX, scaleY, ok := cachedRelScale(p.videoW, p.videoH)
	if !ok {
		var err error
		if scaleX, scaleY, ok, err = p.calibrate(p.videoW, p.videoH); err != nil {
			return err
		}
		if ok {
			storeRelScale(p.videoW, p.videoH, scaleX, scaleY)
		} else {
			scaleX, scaleY = 1, 1
		}
	}
	p.calibrated = ok
	p.gainX = float64(p.videoW) / float64(p.w) / scaleX
	p.gainY = float64(p.videoH) / float64(p.h) / scaleY
	return nil
}

// toCorner pushes the relative pointer into the top-left corner.
func (p *mcpPointer) toCorner() error {
	for range mcpRelHome {
		if err := rpcRelMouseReport(-127, -127, 0); err != nil {
			return err
		}
	}
	p.homed, p.cx, p.cy = true, 0, 0
	return nil
}

// stepTo moves the relative pointer to a position in counts, in short steps
// along a straight line.
func (p *mcpPointer) stepTo(tx, ty int, buttons uint8) error {
	for p.cx != tx || p.cy != ty {
		dx, dy := relStep(tx-p.cx, ty-p.cy)
		if err := rpcRelMouseReport(int8(dx), int8(dy), buttons); err != nil {
			return err
		}
		p.cx += dx
		p.cy += dy
		time.Sleep(mcpRelInterval)
	}
	return nil
}

// relStep is the next step of a relative move by dx,dy: about mcpRelStep
// counts long in any direction. Hosts accelerate by speed, so a diagonal move
// must not be faster than a straight one, or the calibration, which moves
// diagonally, would not fit other directions.
func relStep(dx, dy int) (int, int) {
	length := math.Hypot(float64(dx), float64(dy))
	if length <= mcpRelStep {
		return dx, dy
	}
	f := mcpRelStep / length
	return int(math.Round(float64(dx) * f)), int(math.Round(float64(dy) * f))
}

// press sets the buttons without moving.
func (p *mcpPointer) press(buttons uint8) error {
	if p.relative {
		return rpcRelMouseReport(0, 0, buttons)
	}
	return rpcAbsMouseReport(p.ax, p.ay, buttons)
}

// nudge moves about one pixel towards x,y with buttons held. The host uses
// the first movement of a drag to start it and does not move the dragged
// object by it.
func (p *mcpPointer) nudge(x, y float64, buttons uint8) error {
	towards := func(from, to float64, step int) int {
		switch {
		case to > from:
			return step
		case to < from:
			return -step
		}
		return 0
	}
	if p.relative {
		dx := towards(float64(p.cx), x*p.gainX, 1)
		dy := towards(float64(p.cy), y*p.gainY, 1)
		p.cx += dx
		p.cy += dy
		return rpcRelMouseReport(int8(dx), int8(dy), buttons)
	}
	tx, ty, err := absPosition(x, y, p.w, p.h)
	if err != nil {
		return err
	}
	p.ax += towards(float64(p.ax), float64(tx), mcpDragNudge)
	p.ay += towards(float64(p.ay), float64(ty), mcpDragNudge)
	return rpcAbsMouseReport(p.ax, p.ay, buttons)
}

// note is added to a tool's answer when the relative mouse was used.
func (p *mcpPointer) note() string {
	if !p.relative {
		return ""
	}
	if p.calibrated {
		return " The absolute mouse is disabled, so the relative mouse moved the pointer from the top-left corner, " +
			"scaled as measured on this host. It is less exact than the absolute mouse: check the position with a screenshot where precision matters."
	}
	return " The absolute mouse is disabled, so the relative mouse moved the pointer from the top-left corner. " +
		"Measuring how far the host moves the pointer failed, so the position may be off: check it with a screenshot before relying on it."
}

// absPosition maps a position on a w x h screen to the absolute mouse range.
// A whole number aims at the center of that pixel, so x=0 hits the first
// pixel and x=w-1 the last one; fractions move in between.
func absPosition(x, y float64, w, h int) (int, int, error) {
	if w <= 0 || h <= 0 {
		return 0, 0, errors.New("the screen size is unknown because there is no video signal")
	}
	if x < 0 || x > float64(w-1) || y < 0 || y > float64(h-1) {
		return 0, 0, fmt.Errorf("position %g,%g is outside the %dx%d screen", x, y, w, h)
	}
	ax := int((x + 0.5) / float64(w) * absMouseMax)
	ay := int((y + 0.5) / float64(h) * absMouseMax)
	return ax, ay, nil
}

func mouseButton(name string) (uint8, error) {
	switch strings.ToLower(name) {
	case "", "left":
		return 1, nil
	case "right":
		return 2, nil
	case "middle":
		return 4, nil
	}
	return 0, fmt.Errorf("unknown mouse button %q, use left, right or middle", name)
}

type mcpPoint struct {
	X float64 `json:"x" jsonschema:"horizontal position in pixels of the full screenshot; fractions allowed"`
	Y float64 `json:"y" jsonschema:"vertical position in pixels of the full screenshot; fractions allowed"`
}

type mcpMoveInput struct {
	mcpPoint
	mcpAfter
}

func (s mcpScreen) moveMouse(_ context.Context, _ *mcp.CallToolRequest, in mcpMoveInput) (*mcp.CallToolResult, any, error) {
	p, err := s.pointer()
	if err != nil {
		return nil, nil, err
	}
	if err := p.moveTo(in.X, in.Y, 0); err != nil {
		return nil, nil, err
	}
	return mcpDone("Moved the pointer to %g,%g.%s", in.X, in.Y, p.note())
}

type mcpClickInput struct {
	mcpPoint
	mcpAfter
	Button string `json:"button,omitempty" jsonschema:"left, right or middle, default left"`
	Double bool   `json:"double,omitempty" jsonschema:"click twice"`
}

func (s mcpScreen) click(_ context.Context, _ *mcp.CallToolRequest, in mcpClickInput) (*mcp.CallToolResult, any, error) {
	button, err := mouseButton(in.Button)
	if err != nil {
		return nil, nil, err
	}
	p, err := s.pointer()
	if err != nil {
		return nil, nil, err
	}

	clicks := 1
	if in.Double {
		clicks = 2
	}
	if err := p.moveTo(in.X, in.Y, 0); err != nil {
		return nil, nil, err
	}
	for i := range clicks {
		if i > 0 {
			time.Sleep(mcpDoubleClick)
		} else {
			time.Sleep(mcpKeyDelay)
		}
		if err := p.press(button); err != nil {
			return nil, nil, err
		}
		time.Sleep(mcpClickHold)
		if err := p.press(0); err != nil {
			return nil, nil, err
		}
	}
	action := "Clicked"
	if in.Double {
		action = "Double-clicked"
	}
	return mcpDone("%s the %s button at %g,%g.%s", action, buttonName(in.Button), in.X, in.Y, p.note())
}

func buttonName(name string) string {
	if name == "" {
		return "left"
	}
	return strings.ToLower(name)
}

type mcpDragInput struct {
	FromX  float64 `json:"from_x" jsonschema:"start position, horizontal, in pixels of the full screenshot"`
	FromY  float64 `json:"from_y" jsonschema:"start position, vertical, in pixels of the full screenshot"`
	ToX    float64 `json:"to_x" jsonschema:"end position, horizontal, in pixels of the full screenshot"`
	ToY    float64 `json:"to_y" jsonschema:"end position, vertical, in pixels of the full screenshot"`
	Button string  `json:"button,omitempty" jsonschema:"left, right or middle, default left"`
	mcpAfter
}

func (s mcpScreen) drag(_ context.Context, _ *mcp.CallToolRequest, in mcpDragInput) (*mcp.CallToolResult, any, error) {
	button, err := mouseButton(in.Button)
	if err != nil {
		return nil, nil, err
	}
	p, err := s.pointer()
	if err != nil {
		return nil, nil, err
	}
	if err := p.check(in.ToX, in.ToY); err != nil {
		return nil, nil, err
	}

	if err := p.moveTo(in.FromX, in.FromY, 0); err != nil {
		return nil, nil, err
	}
	time.Sleep(mcpKeyDelay)
	if err := p.press(button); err != nil {
		return nil, nil, err
	}
	time.Sleep(mcpKeyDelay)
	if err := p.nudge(in.ToX, in.ToY, button); err != nil {
		_ = p.press(0)
		return nil, nil, err
	}
	// Intermediate positions let the host recognize the movement as a drag.
	for i := 1; i <= mcpDragSteps; i++ {
		time.Sleep(mcpKeyDelay)
		x := in.FromX + (in.ToX-in.FromX)*float64(i)/mcpDragSteps
		y := in.FromY + (in.ToY-in.FromY)*float64(i)/mcpDragSteps
		if err := p.moveTo(x, y, button); err != nil {
			_ = p.press(0)
			return nil, nil, err
		}
	}
	time.Sleep(mcpClickHold)
	if err := p.press(0); err != nil {
		return nil, nil, err
	}
	return mcpDone("Dragged with the %s button from %g,%g to %g,%g.%s", buttonName(in.Button), in.FromX, in.FromY, in.ToX, in.ToY, p.note())
}

type mcpScrollInput struct {
	mcpPoint
	mcpAfter
	DeltaY int `json:"delta_y,omitempty" jsonschema:"wheel notches, positive turns the wheel down (the host may reverse the scroll direction)"`
	DeltaX int `json:"delta_x,omitempty" jsonschema:"wheel notches, positive turns the wheel right (the host may reverse the scroll direction)"`
}

func (s mcpScreen) scroll(_ context.Context, _ *mcp.CallToolRequest, in mcpScrollInput) (*mcp.CallToolResult, any, error) {
	if in.DeltaX < -mcpMaxScroll || in.DeltaX > mcpMaxScroll || in.DeltaY < -mcpMaxScroll || in.DeltaY > mcpMaxScroll {
		return nil, nil, fmt.Errorf("scroll at most %d notches per call; call scroll again for more, or press PageDown, PageUp, Home or End", mcpMaxScroll)
	}
	p, err := s.pointer()
	if err != nil {
		return nil, nil, err
	}
	if err := p.moveTo(in.X, in.Y, 0); err != nil {
		return nil, nil, err
	}

	sign := func(v int) int8 {
		switch {
		case v > 0:
			return 1
		case v < 0:
			return -1
		}
		return 0
	}
	// One report per notch, as from a mouse wheel. HID counts up as positive.
	dy, dx := -in.DeltaY, in.DeltaX
	for dy != 0 || dx != 0 {
		time.Sleep(mcpKeyDelay)
		sy, sx := sign(dy), sign(dx)
		if err := rpcWheelReport(sy, sx); err != nil {
			return nil, nil, err
		}
		dy -= int(sy)
		dx -= int(sx)
	}
	return mcpDone("Turned the wheel delta_y=%d, delta_x=%d at %g,%g. Take a screenshot to see which way the host scrolled.%s", in.DeltaY, in.DeltaX, in.X, in.Y, p.note())
}

// keyAliases are short forms models tend to use for the modifier keys.
var keyAliases = map[string]string{
	"ctrl":    "ControlLeft",
	"control": "ControlLeft",
	"shift":   "ShiftLeft",
	"alt":     "AltLeft",
	"option":  "AltLeft",
	"altgr":   "AltRight",
	"cmd":     "MetaLeft",
	"command": "MetaLeft",
	"meta":    "MetaLeft",
	"win":     "MetaLeft",
	"windows": "MetaLeft",
	"super":   "MetaLeft",
	"esc":     "Escape",
	"return":  "Enter",
	"del":     "Delete",
}

// keyStroke builds one keyboard report from key names. Modifier keys go into
// the modifier byte, the others into the six key slots.
func keyStroke(names []string) (keyboard.Stroke, error) {
	var stroke keyboard.Stroke
	for _, name := range names {
		if alias, ok := keyAliases[strings.ToLower(name)]; ok {
			name = alias
		}
		code, ok := keyboard.KeyCode(name)
		if !ok {
			return stroke, fmt.Errorf("unknown key %q", name)
		}
		if bit, ok := keyboard.ModifierBit(code); ok {
			stroke.Modifier |= bit
			continue
		}
		stroke.Keys = append(stroke.Keys, code)
	}
	if len(stroke.Keys) > hidrpc.HidKeyBufferSize {
		return stroke, fmt.Errorf("at most %d keys besides modifiers can be pressed together", hidrpc.HidKeyBufferSize)
	}
	return stroke, nil
}

// mcpKeyboardLock runs one keyboard tool call at a time. Stateless MCP calls
// can arrive in parallel, and a new macro cancels the running one, so two
// type_text calls would otherwise cut each other off.
var mcpKeyboardLock sync.Mutex

// runKeyStrokes presses and releases each stroke in turn through the keyboard
// macro runner the paste dialog uses.
func runKeyStrokes(strokes []keyboard.Stroke, hold time.Duration) error {
	if err := requireHID(); err != nil {
		return err
	}
	mcpKeyboardLock.Lock()
	defer mcpKeyboardLock.Unlock()
	steps := make([]hidrpc.KeyboardMacroStep, 0, 2*len(strokes))
	for _, s := range strokes {
		keys := make([]byte, hidrpc.HidKeyBufferSize)
		copy(keys, s.Keys)
		steps = append(steps,
			hidrpc.KeyboardMacroStep{Modifier: s.Modifier, Keys: keys, Delay: uint16(hold.Milliseconds())},
			hidrpc.KeyboardMacroStep{Keys: make([]byte, hidrpc.HidKeyBufferSize), Delay: uint16(mcpKeyDelay.Milliseconds())},
		)
	}
	return rpcExecuteKeyboardMacro(steps)
}

type mcpPressKeysInput struct {
	Keys   []string `json:"keys" jsonschema:"key names pressed together, modifiers included"`
	HoldMS int      `json:"hold_ms,omitempty" jsonschema:"how long to hold the keys in milliseconds, default 20, at most 30000 (e.g. Option through a Mac's boot)"`
	mcpAfter
}

func mcpPressKeys(_ context.Context, _ *mcp.CallToolRequest, in mcpPressKeysInput) (*mcp.CallToolResult, any, error) {
	if len(in.Keys) == 0 {
		return nil, nil, errors.New("no keys given")
	}
	hold := mcpKeyDelay
	if in.HoldMS > 0 {
		hold = time.Duration(in.HoldMS) * time.Millisecond
	}
	if hold > mcpMaxHold {
		return nil, nil, fmt.Errorf("keys can be held for at most %s", mcpMaxHold)
	}
	stroke, err := keyStroke(in.Keys)
	if err != nil {
		return nil, nil, err
	}
	if err := runKeyStrokes([]keyboard.Stroke{stroke}, hold); err != nil {
		return nil, nil, err
	}
	return mcpDone("Pressed %s for %d ms.", strings.Join(in.Keys, "+"), hold.Milliseconds())
}

type mcpTypeTextInput struct {
	Text   string `json:"text" jsonschema:"the text to type"`
	Layout string `json:"layout,omitempty" jsonschema:"keyboard layout of the host, such as de-DE for Windows and Linux or de-DE-macos for a Mac; default the layout set in the JetKVM"`
	mcpAfter
}

func mcpTypeText(_ context.Context, _ *mcp.CallToolRequest, in mcpTypeTextInput) (*mcp.CallToolResult, any, error) {
	if len([]rune(in.Text)) > mcpMaxTextLength {
		return nil, nil, fmt.Errorf("text is longer than %d characters, split it up", mcpMaxTextLength)
	}
	code := in.Layout
	if code == "" {
		code = config.KeyboardLayout
	}
	layout, ok := keyboard.FindLayout(code)
	if !ok {
		if in.Layout != "" {
			return nil, nil, fmt.Errorf("unknown layout %q, available: %s", in.Layout, strings.Join(keyboard.LayoutCodes(), ", "))
		}
		layout, _ = keyboard.FindLayout(keyboard.DefaultLayout)
	}
	strokes, err := layout.Strokes(in.Text)
	if err != nil {
		return nil, nil, err
	}
	if err := runKeyStrokes(strokes, mcpKeyDelay); err != nil {
		return nil, nil, err
	}
	return mcpDone("Typed %d characters with the %s layout (%s).", len([]rune(in.Text)), layout.IsoCode, layout.Name)
}

type mcpState struct {
	Video            native.VideoState        `json:"video"`
	USB              string                   `json:"usb"`
	KeyboardLayout   string                   `json:"keyboard_layout"`
	KeyboardLEDs     *usbgadget.KeyboardState `json:"keyboard_leds,omitempty"`
	VirtualMedia     *VirtualMediaState       `json:"virtual_media,omitempty"`
	PowerExtension   string                   `json:"power_extension,omitempty"`
	ATX              *ATXState                `json:"atx,omitempty"`
	DC               *DCPowerState            `json:"dc,omitempty"`
	Jiggler          bool                     `json:"jiggler"`
	AvailableLayouts []string                 `json:"available_layouts"`
}

func mcpGetState(_ context.Context, _ *mcp.CallToolRequest, _ mcpEmpty) (*mcp.CallToolResult, mcpState, error) {
	state := mcpState{
		Video:            lastVideoState,
		KeyboardLayout:   config.KeyboardLayout,
		PowerExtension:   config.ActiveExtension,
		Jiggler:          config.JigglerEnabled,
		AvailableLayouts: keyboard.LayoutCodes(),
	}
	if gadget != nil {
		state.USB = rpcGetUSBState()
		leds := rpcGetKeyboardLedState()
		state.KeyboardLEDs = &leds
	}
	state.VirtualMedia, _ = rpcGetVirtualMediaState()
	switch config.ActiveExtension {
	case "atx-power":
		atx, _ := rpcGetATXState()
		state.ATX = &atx
	case "dc-power":
		dc, _ := rpcGetDCPowerState()
		state.DC = &dc
	}
	return nil, state, nil
}

type mcpJigglerInput struct {
	Enabled *bool `json:"enabled,omitempty" jsonschema:"true to turn the jiggler on, false to turn it off; leave out to only report the state"`
}

func mcpJiggler(_ context.Context, _ *mcp.CallToolRequest, in mcpJigglerInput) (*mcp.CallToolResult, any, error) {
	if in.Enabled != nil {
		if err := rpcSetJigglerState(*in.Enabled); err != nil {
			return nil, nil, err
		}
	}
	if !config.JigglerEnabled {
		return mcpDone("The jiggler is off.")
	}
	jiggler := config.JigglerConfig
	if jiggler == nil {
		return mcpDone("The jiggler is on.")
	}
	text := fmt.Sprintf("The jiggler is on: it moves the mouse after %d seconds without input, checked on the schedule %q",
		jiggler.InactivityLimitSeconds, jiggler.ScheduleCronTab)
	if jiggler.Timezone != "" {
		text += " in " + jiggler.Timezone
	}
	text += "."
	if gadget != nil && !gadget.HasRelativeMouse() {
		text += " It needs the relative mouse, which is disabled in the JetKVM's USB settings, so it has no effect."
	}
	return mcpDone("%s", text)
}

func mcpWakeHost(_ context.Context, _ *mcp.CallToolRequest, _ mcpEmpty) (*mcp.CallToolResult, any, error) {
	if err := requireHID(); err != nil {
		return nil, nil, err
	}
	if err := rpcWakeHost(); err != nil {
		return nil, nil, err
	}
	// The USB state does not tell whether the host sleeps: a sleeping Mac
	// keeps the port configured and still wakes from this signal.
	return mcpDone("Sent a USB remote wakeup. A sleeping host that allows USB wakeup wakes within a few seconds; " +
		"a display that only went dark wakes from a key press such as ShiftLeft. Take a screenshot to check.")
}

type mcpPowerInput struct {
	Action string `json:"action" jsonschema:"power-short, power-long or reset with the ATX extension; on or off with the DC extension"`
}

func mcpPower(_ context.Context, _ *mcp.CallToolRequest, in mcpPowerInput) (*mcp.CallToolResult, any, error) {
	switch config.ActiveExtension {
	case "atx-power":
		if err := rpcSetATXPowerAction(in.Action); err != nil {
			return nil, nil, err
		}
		done := map[string]string{
			"power-short": "Pressed the power button.",
			"power-long":  "Held the power button for five seconds.",
			"reset":       "Pressed the reset button.",
		}
		return mcpDone("%s", done[in.Action])
	case "dc-power":
		switch in.Action {
		case "on", "off":
			if err := rpcSetDCPowerState(in.Action == "on"); err != nil {
				return nil, nil, err
			}
			return mcpDone("Switched the host's power supply %s.", in.Action)
		}
		return nil, nil, fmt.Errorf("invalid action %q for the DC extension, use on or off", in.Action)
	}
	return nil, nil, errors.New("no power extension is active on the JetKVM")
}

type mcpMountInput struct {
	URL  string `json:"url,omitempty" jsonschema:"HTTP or HTTPS URL of the image"`
	File string `json:"file,omitempty" jsonschema:"name of an image in the JetKVM's storage"`
	Mode string `json:"mode,omitempty" jsonschema:"CDROM or Disk, default CDROM"`
}

func mcpMountMedia(_ context.Context, _ *mcp.CallToolRequest, in mcpMountInput) (*mcp.CallToolResult, any, error) {
	mode := CDROM
	switch strings.ToLower(in.Mode) {
	case "", "cdrom":
	case "disk":
		mode = Disk
	default:
		return nil, nil, fmt.Errorf("unknown mode %q, use CDROM or Disk", in.Mode)
	}
	var source string
	var err error
	switch {
	case in.URL != "" && in.File != "":
		return nil, nil, errors.New("give either url or file, not both")
	case in.URL != "":
		source, err = in.URL, rpcMountWithHTTP(in.URL, mode)
	case in.File != "":
		source, err = in.File, rpcMountWithStorage(in.File, mode)
	default:
		return nil, nil, errors.New("give url or file")
	}
	if err != nil {
		return nil, nil, err
	}
	return mcpDone("Attached %s to the host as a USB %s.", source, mode)
}

func mcpUnmountMedia(_ context.Context, _ *mcp.CallToolRequest, _ mcpEmpty) (*mcp.CallToolResult, any, error) {
	if err := rpcUnmountImage(); err != nil {
		return nil, nil, err
	}
	return mcpDone("Detached the virtual USB drive from the host.")
}
