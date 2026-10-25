//go:build linux && arm

package kvm

import (
	"context"
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
	token := setUpAPITest(t)
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
	for _, want := range []string{"screenshot", "move_mouse", "click", "drag", "scroll", "press_keys", "type_text", "get_state", "wake_host", "power", "mount_media", "unmount_media"} {
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

func TestAbsPosition(t *testing.T) {
	tests := []struct {
		x, y, w, h     int
		wantX, wantY   int
		wantErrContain string
	}{
		{0, 0, 1920, 1080, 8, 15, ""},
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
				t.Errorf("absPosition(%d,%d,%d,%d) error %v, want %q", tt.x, tt.y, tt.w, tt.h, err, tt.wantErrContain)
			}
			continue
		}
		if err != nil || x != tt.wantX || y != tt.wantY {
			t.Errorf("absPosition(%d,%d,%d,%d) = %d,%d,%v, want %d,%d", tt.x, tt.y, tt.w, tt.h, x, y, err, tt.wantX, tt.wantY)
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
