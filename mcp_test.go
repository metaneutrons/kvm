//go:build linux && arm

package kvm

import (
	"context"
	"encoding/json"
	"image"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type bearerTransport struct{ token string }

func (b bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(req)
}

func connectMCPTestClient(t *testing.T) *mcp.ClientSession {
	t.Helper()
	return connectMCPTestClientTo(t, "/api/mcp")
}

func connectMCPTestClientTo(t *testing.T, path string) *mcp.ClientSession {
	t.Helper()
	token := setUpAPITest(t)
	r := newAPITestRouter()
	r.Group("/api", apiCrossOriginMiddleware, apiAuthMiddleware()).Any("/mcp", handleMCP)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	client := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:   srv.URL + path,
		HTTPClient: &http.Client{Transport: bearerTransport{token}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func TestMCPListsTools(t *testing.T) {
	session := connectMCPTestClient(t)
	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	for _, want := range []string{"screenshot", "move_mouse", "click", "drag", "scroll", "press_keys", "type_text", "get_state", "wait", "wake_host", "jiggler", "power", "mount_media", "unmount_media"} {
		if !slices.Contains(names, want) {
			t.Errorf("tool %s is missing from %v", want, names)
		}
	}
}

func TestMCPToolErrorsReachTheModel(t *testing.T) {
	session := connectMCPTestClient(t)
	tests := []struct {
		tool string
		args map[string]any
		want string
	}{
		{"type_text", map[string]any{"text": "hi", "layout": "xx-XX"}, "unknown layout"},
		{"type_text", map[string]any{"text": "a\tb", "layout": "en-US"}, "cannot type"},
		{"press_keys", map[string]any{"keys": []string{"Ctrl", "NoSuchKey"}}, "unknown key"},
		{"click", map[string]any{"x": 1, "y": 1, "button": "fourth"}, "unknown mouse button"},
		{"mount_media", map[string]any{}, "give url or file"},
		{"screenshot", map[string]any{"quality": 100}, "quality"},
	}
	for _, tt := range tests {
		res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: tt.tool, Arguments: tt.args})
		if err != nil {
			t.Errorf("%s: protocol error %v", tt.tool, err)
			continue
		}
		text := ""
		if len(res.Content) > 0 {
			if tc, ok := res.Content[0].(*mcp.TextContent); ok {
				text = tc.Text
			}
		}
		if !res.IsError || !strings.Contains(text, tt.want) {
			t.Errorf("%s %v: isError=%v %q, want an error containing %q", tt.tool, tt.args, res.IsError, text, tt.want)
		}
	}
}

func TestMCPRequiresAuth(t *testing.T) {
	setUpAPITest(t)
	r := newAPITestRouter()
	r.Group("/api", apiCrossOriginMiddleware, apiAuthMiddleware()).Any("/mcp", handleMCP)
	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`

	if w := doAPIRequest(r, http.MethodPost, "/api/mcp", initialize, "", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("without credentials: status %d, want 401", w.Code)
	}

	// The login cookie works for /api, so a cross-site browser request must
	// not get through with it.
	req := httptest.NewRequest(http.MethodPost, "/api/mcp", strings.NewReader(initialize))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Origin", "https://evil.example")
	req.AddCookie(&http.Cookie{Name: "authToken", Value: "cookie-token"})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("cross-site request with cookie: status %d, want 403", w.Code)
	}
}

// testFrame returns a frame of uniform brightness with dark squares of size
// px at the given points.
func testFrame(w, h int, base uint8, size int, at ...image.Point) *image.YCbCr {
	img := image.NewYCbCr(image.Rect(0, 0, w, h), image.YCbCrSubsampleRatio420)
	for i := range img.Y {
		img.Y[i] = base
	}
	for _, p := range at {
		for y := p.Y; y < p.Y+size && y < h; y++ {
			for x := p.X; x < p.X+size && x < w; x++ {
				img.Y[img.YOffset(x, y)] = 0
			}
		}
	}
	return img
}

func TestChangedAreas(t *testing.T) {
	// The pointer is in the corner, at the first point and at the second
	// point; the third frame also has a small speck of noise and a large
	// change elsewhere, such as a clock.
	corner := testFrame(200, 150, 200, 8, image.Point{})
	first := testFrame(200, 150, 200, 8, image.Point{X: 61, Y: 41})
	second := testFrame(200, 150, 200, 8, image.Point{X: 181, Y: 121}, image.Point{X: 10, Y: 100})
	second.Y[second.YOffset(5, 5)] = 0

	atFirst := changedAreas(first, corner, second)
	if len(atFirst) != 1 || atFirst[0] != (image.Point{X: 61, Y: 41}) {
		t.Fatalf("areas at the first point = %v", atFirst)
	}
	atSecond := changedAreas(second, corner, first)
	if len(atSecond) != 2 {
		t.Fatalf("areas at the second point = %v, want the pointer and the clock", atSecond)
	}

	// 60 pixels per 100 counts, offset 1 pixel; the clock does not fit.
	sx, sy, ok := pointerScale(atFirst, atSecond, 100, 300)
	if !ok || math.Abs(sx-0.6) > 1e-9 || math.Abs(sy-0.4) > 1e-9 {
		t.Fatalf("pointerScale = %g, %g, %v, want 0.6, 0.4, true", sx, sy, ok)
	}

	// Areas that move backwards or sit far off the line are no pointer.
	if _, _, ok := pointerScale([]image.Point{{X: 100, Y: 100}}, []image.Point{{X: 50, Y: 50}}, 100, 300); ok {
		t.Error("pointerScale accepted a pointer moving backwards")
	}
	if _, _, ok := pointerScale([]image.Point{{X: 150, Y: 150}}, []image.Point{{X: 200, Y: 200}}, 100, 300); ok {
		t.Error("pointerScale accepted a hotspot offset of 125 pixels")
	}
}

func TestRelStep(t *testing.T) {
	tests := []struct {
		dx, dy, wantX, wantY int
	}{
		{0, 0, 0, 0},
		{5, -3, 5, -3},
		{8, 0, 8, 0},
		{8, 8, 6, 6},
		{100, 0, 8, 0},
		{-100, 50, -7, 4},
		{30, -90, 3, -8},
	}
	for _, tt := range tests {
		x, y := relStep(tt.dx, tt.dy)
		if x != tt.wantX || y != tt.wantY {
			t.Errorf("relStep(%d, %d) = %d, %d, want %d, %d", tt.dx, tt.dy, x, y, tt.wantX, tt.wantY)
		}
	}

	// Stepping always arrives, also where the shorter axis rounds to zero.
	for _, d := range [][2]int{{1000, 1}, {-7, 999}, {123, -456}} {
		cx, cy, n := 0, 0, 0
		for cx != d[0] || cy != d[1] {
			sx, sy := relStep(d[0]-cx, d[1]-cy)
			cx, cy, n = cx+sx, cy+sy, n+1
			if n > 1000 {
				t.Fatalf("stepping to %v did not arrive", d)
			}
		}
	}
}

func TestAbsPosition(t *testing.T) {
	tests := []struct {
		x, y           float64
		w, h           int
		wantX, wantY   int
		wantErrContain string
	}{
		{0, 0, 1920, 1080, 8, 15, ""},
		// A fraction lands between two pixels.
		{959.5, 539.5, 1920, 1080, 16383, 16383, ""},
		{959, 539, 1920, 1080, 16374, 16368, ""},
		{1919, 1079, 1920, 1080, 32758, 32751, ""},
		{1920, 0, 1920, 1080, 0, 0, "outside"},
		{-1, 0, 1920, 1080, 0, 0, "outside"},
		{0, 0, 0, 0, 0, 0, "no video signal"},
	}
	for _, tt := range tests {
		x, y, err := absPosition(tt.x, tt.y, tt.w, tt.h)
		if tt.wantErrContain != "" {
			if err == nil || !strings.Contains(err.Error(), tt.wantErrContain) {
				t.Errorf("absPosition(%g,%g,%d,%d) error %v, want %q", tt.x, tt.y, tt.w, tt.h, err, tt.wantErrContain)
			}
			continue
		}
		if err != nil || x != tt.wantX || y != tt.wantY {
			t.Errorf("absPosition(%g,%g,%d,%d) = %d,%d,%v, want %d,%d", tt.x, tt.y, tt.w, tt.h, x, y, err, tt.wantX, tt.wantY)
		}
	}
}

func TestKeyStroke(t *testing.T) {
	stroke, err := keyStroke([]string{"Ctrl", "shift", "MetaLeft", "KeyC"})
	if err != nil {
		t.Fatal(err)
	}
	if stroke.Modifier != 0x01|0x02|0x08 || !slices.Equal(stroke.Keys, []byte{0x06}) {
		t.Errorf("got modifier %#x keys %v", stroke.Modifier, stroke.Keys)
	}
	if _, err := keyStroke([]string{"KeyA", "KeyB", "KeyC", "KeyD", "KeyE", "KeyF", "KeyG"}); err == nil {
		t.Error("seven keys were accepted")
	}
}

func TestMCPScreenSize(t *testing.T) {
	tests := []struct {
		screen             mcpScreen
		w, h, wantW, wantH int
	}{
		{1280, 1920, 1080, 1280, 720},
		{1280, 1280, 1024, 1280, 1024},
		{1280, 1024, 768, 1024, 768},
		{1280, 2560, 1440, 1280, 720},
		{1280, 1920, 1200, 1280, 800},
		{0, 1920, 1080, 1920, 1080},
		{1600, 1920, 1080, 1600, 900},
	}
	for _, tt := range tests {
		if w, h := tt.screen.size(tt.w, tt.h); w != tt.wantW || h != tt.wantH {
			t.Errorf("mcpScreen(%d).size(%d,%d) = %d,%d, want %d,%d", tt.screen, tt.w, tt.h, w, h, tt.wantW, tt.wantH)
		}
	}
}

func TestMCPSnapshotOptions(t *testing.T) {
	screen := mcpScreen(mcpDefaultScreenWidth)
	// The whole 1080p screen is scaled to 1280x720.
	opts, err := screen.snapshotOptions(80, nil, 1920, 1080)
	if err != nil || opts.Width != 1280 || opts.Height != 720 || opts.CropWidth != 0 {
		t.Errorf("full screen: %+v, %v", opts, err)
	}
	// A screen that fits is captured as it is.
	opts, err = screen.snapshotOptions(80, nil, 1024, 768)
	if err != nil || opts.Width != 0 || opts.Height != 0 {
		t.Errorf("small screen: %+v, %v", opts, err)
	}
	// A region in screen coordinates maps to video pixels at full resolution.
	opts, err = screen.snapshotOptions(80, &mcpRegion{X: 640, Y: 360, Width: 320, Height: 180}, 1920, 1080)
	if err != nil || opts.CropX != 960 || opts.CropY != 540 || opts.CropWidth != 480 || opts.CropHeight != 270 || opts.Width != 0 {
		t.Errorf("region: %+v, %v", opts, err)
	}
	// A wide region is not delivered wider than the screenshot.
	opts, err = screen.snapshotOptions(80, &mcpRegion{X: 0, Y: 0, Width: 1280, Height: 360}, 1920, 1080)
	if err != nil || opts.CropWidth != 1920 || opts.Width != 1280 || opts.Height != 360 {
		t.Errorf("wide region: %+v, %v", opts, err)
	}
	for _, bad := range []mcpRegion{
		{X: 1200, Y: 0, Width: 100, Height: 100},
		{X: -1, Y: 0, Width: 100, Height: 100},
		{X: 0, Y: 0, Width: 8, Height: 100},
		{X: 0, Y: 700, Width: 100, Height: 30},
	} {
		if _, err := screen.snapshotOptions(80, &bad, 1920, 1080); err == nil {
			t.Errorf("region %+v was accepted", bad)
		}
	}
}

func TestParseMCPScreen(t *testing.T) {
	for raw, want := range map[string]mcpScreen{"": 1280, "native": 0, "1920": 1920, "320": 320} {
		if got, err := parseMCPScreen(raw); err != nil || got != want {
			t.Errorf("parseMCPScreen(%q) = %d, %v, want %d", raw, got, err, want)
		}
	}
	for _, raw := range []string{"0", "100", "8000", "wide", "-1280"} {
		if _, err := parseMCPScreen(raw); err == nil {
			t.Errorf("parseMCPScreen(%q) was accepted", raw)
		}
	}
}

func TestMCPScreenWidthParameter(t *testing.T) {
	description := func(path string) string {
		session := connectMCPTestClientTo(t, path)
		res, err := session.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, tool := range res.Tools {
			if tool.Name == "screenshot" {
				return tool.Description
			}
		}
		t.Fatalf("%s has no screenshot tool", path)
		return ""
	}
	if d := description("/api/mcp"); !strings.Contains(d, "at most 1280 pixels") {
		t.Errorf("default description: %q", d)
	}
	if d := description("/api/mcp?screen_width=native"); !strings.Contains(d, "own resolution") {
		t.Errorf("native description: %q", d)
	}
	if d := description("/api/mcp?screen_width=1600"); !strings.Contains(d, "at most 1600 pixels") {
		t.Errorf("1600 description: %q", d)
	}

	token := setUpAPITest(t)
	r := newAPITestRouter()
	r.Group("/api", apiCrossOriginMiddleware, apiAuthMiddleware()).Any("/mcp", handleMCP)
	w := doAPIRequest(r, http.MethodPost, "/api/mcp?screen_width=huge", "{}", token, "")
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "screen_width") {
		t.Errorf("invalid screen_width: status %d, %s", w.Code, w.Body.String())
	}
}

func TestScreenDifference(t *testing.T) {
	newThumb := func(fill uint8) *image.YCbCr {
		img := image.NewYCbCr(image.Rect(0, 0, 320, 180), image.YCbCrSubsampleRatio420)
		for i := range img.Y {
			img.Y[i] = fill
		}
		return img
	}
	a := newThumb(100)
	if d := screenDifference(a, newThumb(100)); d != 0 {
		t.Errorf("identical thumbnails differ by %v", d)
	}
	// Encoding noise shifts every pixel a little.
	if d := screenDifference(a, newThumb(110)); d != 0 {
		t.Errorf("noise counts as a change: %v", d)
	}
	// A checkbox of 4x4 pixels being ticked is a change.
	b := newThumb(100)
	for y := 80; y < 84; y++ {
		for x := 150; x < 154; x++ {
			b.Y[b.YOffset(x, y)] = 255
		}
	}
	if d := screenDifference(a, b); d <= mcpChangedShare {
		t.Errorf("a ticked checkbox is not a change: %v", d)
	}
	// A clock's minute digits, 8x4 pixels, ticking leave the screen stable.
	c := newThumb(100)
	for y := 2; y < 6; y++ {
		for x := 300; x < 308; x++ {
			c.Y[c.YOffset(x, y)] = 0
		}
	}
	if d := screenDifference(a, c); d > mcpStableShare {
		t.Errorf("a ticking clock keeps the screen from being stable: %v", d)
	}
	if d := screenDifference(a, image.NewYCbCr(image.Rect(0, 0, 160, 90), image.YCbCrSubsampleRatio420)); d != 1 {
		t.Errorf("thumbnails of different size compare as %v", d)
	}
}

func TestMCPInputToolsOfferScreenshot(t *testing.T) {
	session := connectMCPTestClient(t)
	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		if !slices.Contains([]string{"move_mouse", "click", "drag", "scroll", "press_keys", "type_text"}, tool.Name) {
			continue
		}
		schema, _ := json.Marshal(tool.InputSchema)
		if !strings.Contains(string(schema), `"screenshot"`) {
			t.Errorf("%s has no screenshot argument: %s", tool.Name, schema)
		}
	}
}

func TestMCPViewOnlyToken(t *testing.T) {
	token := setUpAPITest(t)
	config.APITokenScope = apiScopeView
	r := newAPITestRouter()
	r.Group("/api", apiCrossOriginMiddleware, apiAuthMiddleware()).Any("/mcp", handleMCP)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	client := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:   srv.URL + "/api/mcp",
		HTTPClient: &http.Client{Transport: bearerTransport{token}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	if want := []string{"get_state", "screenshot", "wait"}; !slices.Equal(names, want) {
		t.Errorf("view-only tools %v, want %v", names, want)
	}
}
