package kvm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/jpeg"
	"net/http"
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
	mcpMaxScroll   = 50
	mcpMaxHold     = 5 * time.Second
	absMouseMax    = 32767
	// mcpScreenWidth is the widest screen the agent sees. Larger video is
	// scaled down to it, which keeps screenshots small and below the sizes at
	// which clients scale images themselves; the mouse tools use the same
	// coordinates, so they match what the model sees.
	mcpScreenWidth = 1280
)

const mcpInstructions = `This server controls the computer attached to a JetKVM. It sees the computer's HDMI output and acts as its USB keyboard and mouse.
Take a screenshot first. It shows the whole screen, scaled to at most 1280 pixels wide, and states the screen size. All mouse tools take x,y in the pixels of that full screenshot, with 0,0 at the top left, whatever the real video resolution is.
To read small text or check details, take a screenshot with a region: it returns that part of the screen enlarged up to the real resolution. Coordinates for the mouse tools stay those of the full screenshot; the response explains how to convert a point in the zoomed image.
type_text types characters through the host's keyboard layout, which must match the layout the host uses. press_keys sends key combinations by key name.
If the screenshot reports no signal, the display may be asleep: press a key such as ShiftLeft or call wake_host, then take a new screenshot.`

var mcpHandler = sync.OnceValue(func() http.Handler {
	server := newMCPServer()
	return mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true},
	)
})

func handleMCP(c *gin.Context) {
	mcpHandler().ServeHTTP(c.Writer, c.Request)
}

var (
	mcpReadOnly = &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: boolPtr(false)}
	// Input reaches a computer whose state the server cannot see.
	mcpInput = &mcp.ToolAnnotations{DestructiveHint: boolPtr(true), OpenWorldHint: boolPtr(false)}
)

func newMCPServer() *mcp.Server {
	server := mcp.NewServer(
		&mcp.Implementation{Name: "jetkvm", Title: "JetKVM", Version: builtAppVersion},
		&mcp.ServerOptions{Instructions: mcpInstructions},
	)

	mcp.AddTool(server, &mcp.Tool{
		Name: "screenshot",
		Description: "Capture the host's screen as a JPEG image. Without a region it shows the whole screen, scaled to at most 1280 pixels wide; " +
			"its pixels are the coordinates every mouse tool uses. With a region (in those same coordinates) it shows only that part, " +
			"enlarged up to the real video resolution, to read small text. The response states the sizes and how to map a point in a zoomed image back to screen coordinates.",
		Annotations: mcpReadOnly,
	}, mcpScreenshot)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "move_mouse",
		Description: "Move the mouse pointer to a screen position.",
		Annotations: mcpInput,
	}, mcpMoveMouse)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "click",
		Description: "Move the mouse pointer to a screen position and click a button there.",
		Annotations: mcpInput,
	}, mcpClick)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "drag",
		Description: "Press a mouse button at one screen position, move to another and release it there.",
		Annotations: mcpInput,
	}, mcpDrag)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "scroll",
		Description: "Move the mouse pointer to a screen position and turn the scroll wheel there.",
		Annotations: mcpInput,
	}, mcpScroll)
	mcp.AddTool(server, &mcp.Tool{
		Name: "press_keys",
		Description: "Press keys together and release them, for shortcuts and special keys. " +
			`Keys are named after the physical US key position, as in the browser's KeyboardEvent.code: "KeyA", "Digit1", "Enter", "Tab", "Escape", "Backspace", "ArrowUp", "F5", "ControlLeft", "ShiftLeft", "AltLeft", "AltRight", "MetaLeft". ` +
			`Ctrl, Shift, Alt, Cmd, Win and Esc are accepted as short forms. Example: ["ControlLeft", "KeyC"].`,
		Annotations: mcpInput,
	}, mcpPressKeys)
	mcp.AddTool(server, &mcp.Tool{
		Name: "type_text",
		Description: "Type text on the host's keyboard. Characters are mapped through a keyboard layout, " +
			"which must match the layout the host uses. Newlines press Enter. Fails without typing anything if a character is not in the layout.",
		Annotations: mcpInput,
	}, mcpTypeText)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_state",
		Description: "Report the video signal, USB connection, keyboard layout and LEDs, virtual media and power state.",
		Annotations: mcpReadOnly,
	}, mcpGetState)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "wake_host",
		Description: "Send a USB remote wakeup signal to wake the host from sleep.",
		Annotations: mcpInput,
	}, mcpWakeHost)
	mcp.AddTool(server, &mcp.Tool{
		Name: "power",
		Description: "Use the power extension. With the ATX extension: power-short presses the power button, power-long holds it for five seconds to force off, reset presses reset. " +
			"With the DC extension: on and off switch the host's power supply.",
		Annotations: mcpInput,
	}, mcpPower)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "mount_media",
		Description: "Attach an image to the host as a USB drive, either from a URL or from a file in the JetKVM's storage.",
		Annotations: mcpInput,
	}, mcpMountMedia)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "unmount_media",
		Description: "Detach the virtual USB drive from the host.",
		Annotations: mcpInput,
	}, mcpUnmountMedia)

	return server
}

type mcpEmpty struct{}

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

// mcpScreenSize returns the size of the screen the agent works with: the
// video size, scaled down to mcpScreenWidth if it is wider.
func mcpScreenSize(videoWidth, videoHeight int) (int, int) {
	if videoWidth <= mcpScreenWidth {
		return videoWidth, videoHeight
	}
	return mcpScreenWidth, (videoHeight*mcpScreenWidth + videoWidth/2) / videoWidth
}

// mcpSnapshotOptions turns a screenshot request in screen coordinates into
// snapshot options in video pixels. A region is captured at the video's own
// resolution, but never wider than mcpScreenWidth.
func mcpSnapshotOptions(quality int, region *mcpRegion, videoWidth, videoHeight int) (native.SnapshotOptions, error) {
	opts := native.SnapshotOptions{Quality: quality}
	screenWidth, screenHeight := mcpScreenSize(videoWidth, videoHeight)
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
	if opts.CropWidth > mcpScreenWidth {
		opts.Width = mcpScreenWidth
		opts.Height = (opts.CropHeight*mcpScreenWidth + opts.CropWidth/2) / opts.CropWidth
	}
	return opts, nil
}

func mcpScreenshot(_ context.Context, _ *mcp.CallToolRequest, in mcpScreenshotInput) (*mcp.CallToolResult, any, error) {
	quality := in.Quality
	if quality == 0 {
		quality = native.DefaultSnapshotQuality
	}
	if quality < 1 || quality > 99 {
		return nil, nil, errors.New("quality must be between 1 and 99")
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
			if opts, err = mcpSnapshotOptions(quality, in.Region, videoWidth, videoHeight); err != nil {
				return nil, nil, err
			}
		}
		var err error
		image, err = takeVideoSnapshot(opts)
		var noSignal errNoVideoSignal
		switch {
		case errors.As(err, &noSignal):
			return nil, nil, fmt.Errorf("no video signal from the host (%s); the display may be asleep, press a key or call wake_host", noSignal.reason)
		case err != nil:
			return nil, nil, err
		}
		if known || attempt > 0 || lastVideoState.Width <= 0 {
			break
		}
	}

	cfg, err := jpeg.DecodeConfig(bytes.NewReader(image))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read the snapshot: %w", err)
	}
	screenWidth, screenHeight := mcpScreenSize(lastVideoState.Width, lastVideoState.Height)
	var text string
	if in.Region == nil {
		text = fmt.Sprintf("Full screen, %dx%d. Mouse tools take x,y in these pixels.", screenWidth, screenHeight)
	} else {
		r := in.Region
		text = fmt.Sprintf("Zoomed region x=%d y=%d width=%d height=%d of the %dx%d screen, shown at %dx%d (%.1f times). "+
			"Mouse tools still take full-screen coordinates: a point u,v in this image is at x = %d + u*%d/%d, y = %d + v*%d/%d.",
			r.X, r.Y, r.Width, r.Height, screenWidth, screenHeight, cfg.Width, cfg.Height,
			float64(cfg.Width)/float64(r.Width), r.X, r.Width, cfg.Width, r.Y, r.Height, cfg.Height)
	}
	return &mcp.CallToolResult{Content: []mcp.Content{
		&mcp.ImageContent{Data: image, MIMEType: "image/jpeg"},
		&mcp.TextContent{Text: text},
	}}, nil, nil
}

func requireHID() error {
	if gadget == nil || !usbReadyForHidReports() {
		return errors.New("the JetKVM is not connected to the host over USB")
	}
	return nil
}

// toAbsMouse converts screen pixels to the absolute mouse's 0 to 32767 range,
// aiming at the pixel's center.
func toAbsMouse(x, y int) (int, int, error) {
	if err := requireHID(); err != nil {
		return 0, 0, err
	}
	if !gadget.HasAbsoluteMouse() {
		return 0, 0, errors.New("the absolute mouse is disabled in the JetKVM's USB settings")
	}
	screenWidth, screenHeight := mcpScreenSize(lastVideoState.Width, lastVideoState.Height)
	return absPosition(x, y, screenWidth, screenHeight)
}

func absPosition(x, y, w, h int) (int, int, error) {
	if w <= 0 || h <= 0 {
		return 0, 0, errors.New("the screen size is unknown because there is no video signal")
	}
	if x < 0 || x >= w || y < 0 || y >= h {
		return 0, 0, fmt.Errorf("position %d,%d is outside the %dx%d screen", x, y, w, h)
	}
	ax := int((float64(x) + 0.5) / float64(w) * absMouseMax)
	ay := int((float64(y) + 0.5) / float64(h) * absMouseMax)
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
	X int `json:"x" jsonschema:"horizontal position in pixels of the full screenshot"`
	Y int `json:"y" jsonschema:"vertical position in pixels of the full screenshot"`
}

func mcpMoveMouse(_ context.Context, _ *mcp.CallToolRequest, in mcpPoint) (*mcp.CallToolResult, any, error) {
	x, y, err := toAbsMouse(in.X, in.Y)
	if err != nil {
		return nil, nil, err
	}
	return nil, nil, rpcAbsMouseReport(x, y, 0)
}

type mcpClickInput struct {
	mcpPoint
	Button string `json:"button,omitempty" jsonschema:"left, right or middle, default left"`
	Double bool   `json:"double,omitempty" jsonschema:"click twice"`
}

func mcpClick(_ context.Context, _ *mcp.CallToolRequest, in mcpClickInput) (*mcp.CallToolResult, any, error) {
	button, err := mouseButton(in.Button)
	if err != nil {
		return nil, nil, err
	}
	x, y, err := toAbsMouse(in.X, in.Y)
	if err != nil {
		return nil, nil, err
	}

	clicks := 1
	if in.Double {
		clicks = 2
	}
	if err := rpcAbsMouseReport(x, y, 0); err != nil {
		return nil, nil, err
	}
	for i := range clicks {
		if i > 0 {
			time.Sleep(mcpDoubleClick)
		} else {
			time.Sleep(mcpKeyDelay)
		}
		if err := rpcAbsMouseReport(x, y, button); err != nil {
			return nil, nil, err
		}
		time.Sleep(mcpClickHold)
		if err := rpcAbsMouseReport(x, y, 0); err != nil {
			return nil, nil, err
		}
	}
	return nil, nil, nil
}

type mcpDragInput struct {
	FromX  int    `json:"from_x" jsonschema:"start position, horizontal, in pixels of the full screenshot"`
	FromY  int    `json:"from_y" jsonschema:"start position, vertical, in pixels of the full screenshot"`
	ToX    int    `json:"to_x" jsonschema:"end position, horizontal, in pixels of the full screenshot"`
	ToY    int    `json:"to_y" jsonschema:"end position, vertical, in pixels of the full screenshot"`
	Button string `json:"button,omitempty" jsonschema:"left, right or middle, default left"`
}

func mcpDrag(_ context.Context, _ *mcp.CallToolRequest, in mcpDragInput) (*mcp.CallToolResult, any, error) {
	button, err := mouseButton(in.Button)
	if err != nil {
		return nil, nil, err
	}
	fromX, fromY, err := toAbsMouse(in.FromX, in.FromY)
	if err != nil {
		return nil, nil, err
	}
	toX, toY, err := toAbsMouse(in.ToX, in.ToY)
	if err != nil {
		return nil, nil, err
	}

	if err := rpcAbsMouseReport(fromX, fromY, 0); err != nil {
		return nil, nil, err
	}
	time.Sleep(mcpKeyDelay)
	if err := rpcAbsMouseReport(fromX, fromY, button); err != nil {
		return nil, nil, err
	}
	// Intermediate positions let the host recognize the movement as a drag.
	for i := 1; i <= mcpDragSteps; i++ {
		time.Sleep(mcpKeyDelay)
		x := fromX + (toX-fromX)*i/mcpDragSteps
		y := fromY + (toY-fromY)*i/mcpDragSteps
		if err := rpcAbsMouseReport(x, y, button); err != nil {
			_ = rpcAbsMouseReport(x, y, 0)
			return nil, nil, err
		}
	}
	time.Sleep(mcpClickHold)
	return nil, nil, rpcAbsMouseReport(toX, toY, 0)
}

type mcpScrollInput struct {
	mcpPoint
	DeltaY int `json:"delta_y,omitempty" jsonschema:"wheel notches, positive scrolls down"`
	DeltaX int `json:"delta_x,omitempty" jsonschema:"wheel notches, positive scrolls right"`
}

func mcpScroll(_ context.Context, _ *mcp.CallToolRequest, in mcpScrollInput) (*mcp.CallToolResult, any, error) {
	if in.DeltaX < -mcpMaxScroll || in.DeltaX > mcpMaxScroll || in.DeltaY < -mcpMaxScroll || in.DeltaY > mcpMaxScroll {
		return nil, nil, fmt.Errorf("scroll at most %d notches at a time", mcpMaxScroll)
	}
	x, y, err := toAbsMouse(in.X, in.Y)
	if err != nil {
		return nil, nil, err
	}
	if err := rpcAbsMouseReport(x, y, 0); err != nil {
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
	return nil, nil, nil
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
	HoldMS int      `json:"hold_ms,omitempty" jsonschema:"how long to hold the keys in milliseconds, default 20"`
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
	return nil, nil, runKeyStrokes([]keyboard.Stroke{stroke}, hold)
}

type mcpTypeTextInput struct {
	Text   string `json:"text" jsonschema:"the text to type"`
	Layout string `json:"layout,omitempty" jsonschema:"keyboard layout of the host as an ISO code such as en-US or de-DE, default the layout set in the JetKVM"`
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
	return nil, nil, runKeyStrokes(strokes, mcpKeyDelay)
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
	AvailableLayouts []string                 `json:"available_layouts"`
}

func mcpGetState(_ context.Context, _ *mcp.CallToolRequest, _ mcpEmpty) (*mcp.CallToolResult, mcpState, error) {
	state := mcpState{
		Video:            lastVideoState,
		KeyboardLayout:   config.KeyboardLayout,
		PowerExtension:   config.ActiveExtension,
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

func mcpWakeHost(_ context.Context, _ *mcp.CallToolRequest, _ mcpEmpty) (*mcp.CallToolResult, any, error) {
	if err := requireHID(); err != nil {
		return nil, nil, err
	}
	return nil, nil, rpcWakeHost()
}

type mcpPowerInput struct {
	Action string `json:"action" jsonschema:"power-short, power-long or reset with the ATX extension; on or off with the DC extension"`
}

func mcpPower(_ context.Context, _ *mcp.CallToolRequest, in mcpPowerInput) (*mcp.CallToolResult, any, error) {
	switch config.ActiveExtension {
	case "atx-power":
		return nil, nil, rpcSetATXPowerAction(in.Action)
	case "dc-power":
		switch in.Action {
		case "on", "off":
			return nil, nil, rpcSetDCPowerState(in.Action == "on")
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
	switch {
	case in.URL != "" && in.File != "":
		return nil, nil, errors.New("give either url or file, not both")
	case in.URL != "":
		return nil, nil, rpcMountWithHTTP(in.URL, mode)
	case in.File != "":
		return nil, nil, rpcMountWithStorage(in.File, mode)
	}
	return nil, nil, errors.New("give url or file")
}

func mcpUnmountMedia(_ context.Context, _ *mcp.CallToolRequest, _ mcpEmpty) (*mcp.CallToolResult, any, error) {
	return nil, nil, rpcUnmountImage()
}
