//go:build linux && arm

package kvm

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// Do not call the token handlers here: they save the device's real config.

func TestAPIRPCMethodsExistAndNeedNoSession(t *testing.T) {
	sessionType := reflect.TypeOf((*Session)(nil))
	for method := range apiRPCMethods {
		handler, ok := rpcHandlers[method]
		if !ok {
			t.Errorf("%s is allowed for the API but not an RPC method", method)
			continue
		}
		fn := reflect.TypeOf(handler.Func)
		if fn.NumIn() > 0 && fn.In(0) == sessionType {
			t.Errorf("%s needs a WebRTC session, which an API call does not have", method)
		}
	}
}

// setUpAPITest configures one token with all permissions.
func setUpAPITest(t *testing.T) string {
	t.Helper()
	return setUpAPITestPerms(t, "view", "input", "media", "power")
}

func setUpAPITestPerms(t *testing.T, perms ...string) string {
	t.Helper()
	originalConfig := config
	token, err := newAPIToken()
	if err != nil {
		t.Fatal(err)
	}
	config = &Config{
		LocalAuthMode:  "password",
		LocalAuthToken: "cookie-token",
		APITokens:      []APIToken{{ID: "t1", Name: "test", Hash: hashAPIToken(token), Permissions: perms}},
	}
	t.Cleanup(func() { config = originalConfig })
	return token
}

func TestAPITokenValidation(t *testing.T) {
	token := setUpAPITest(t)

	if !strings.HasPrefix(token, apiTokenPrefix) || len(token) < len(apiTokenPrefix)+40 {
		t.Fatalf("unexpected token format %q", token)
	}
	other, _ := newAPIToken()
	if other == token {
		t.Fatal("two generated tokens are equal")
	}
	if _, ok := findAPIToken(token); !ok {
		t.Error("the configured token was rejected")
	}
	for _, bad := range []string{other, "", apiTokenPrefix, strings.TrimPrefix(token, apiTokenPrefix)} {
		if _, ok := findAPIToken(bad); ok {
			t.Errorf("token %q was accepted", bad)
		}
	}

	config.APITokens = nil
	if _, ok := findAPIToken(token); ok {
		t.Error("a token was accepted while none is configured")
	}
}

func TestParseAPIPerms(t *testing.T) {
	perms, err := parseAPIPerms(nil)
	if err != nil || perms != permView {
		t.Errorf("no names: %v, %v; want view only", perms, err)
	}
	perms, err = parseAPIPerms([]string{"power", "input", "input"})
	if err != nil || perms != permView|permInput|permPower {
		t.Errorf("power, input: %v, %v", perms, err)
	}
	if got := perms.names(); !slices.Equal(got, []string{"view", "input", "power"}) {
		t.Errorf("names = %v", got)
	}
	if _, err := parseAPIPerms([]string{"admin"}); err == nil {
		t.Error("an unknown permission was accepted")
	}
	// A stored token with an unknown permission grants nothing.
	if p := (APIToken{Permissions: []string{"view", "admin"}}).perms(); p != 0 {
		t.Errorf("token with an unknown permission has %v", p)
	}
}

func TestAPIRPCMethodPermissions(t *testing.T) {
	for method, perm := range apiRPCMethods {
		if perm == 0 || perm&(perm-1) != 0 || perm&permAll != perm {
			t.Errorf("%s needs %v, which is not one permission", method, perm)
		}
		reads := method == "ping" || strings.HasPrefix(method, "get") || strings.HasPrefix(method, "list")
		if reads != (perm == permView) {
			t.Errorf("%s needs %v; only methods that read state need view", method, perm)
		}
	}
}

func TestNewAPITokenRecord(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	token, rec, err := newAPITokenRecord("  Home Assistant ", []string{"input"}, 30, now)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Name != "Home Assistant" || rec.Hash != hashAPIToken(token) || rec.ID == "" {
		t.Errorf("record %+v", rec)
	}
	if !slices.Equal(rec.Permissions, []string{"view", "input"}) {
		t.Errorf("permissions %v", rec.Permissions)
	}
	if rec.CreatedAt != "2026-10-27T12:00:00Z" || rec.ExpiresAt != "2026-11-26T12:00:00Z" {
		t.Errorf("times %s, %s", rec.CreatedAt, rec.ExpiresAt)
	}
	if _, rec, _ := newAPITokenRecord("a", nil, 0, now); rec.ExpiresAt != "" {
		t.Errorf("a token without expiry has %q", rec.ExpiresAt)
	}
	for _, tt := range []struct {
		name  string
		perms []string
		days  int
	}{
		{"", nil, 0},
		{strings.Repeat("x", apiTokenNameMax+1), nil, 0},
		{"a", []string{"root"}, 0},
		{"a", nil, -1},
		{"a", nil, apiTokenMaxDays + 1},
	} {
		_, _, err := newAPITokenRecord(tt.name, tt.perms, tt.days, now)
		var input apiTokenInputError
		if !errors.As(err, &input) {
			t.Errorf("%+v: error %v, want an input error", tt, err)
		}
	}
}

func TestMigrateLegacyAPIToken(t *testing.T) {
	cfg := &Config{APITokenHash: strings.Repeat("ab", 32), APITokenCreatedAt: "2026-10-01T00:00:00Z"}
	if !migrateLegacyAPIToken(cfg) {
		t.Fatal("nothing migrated")
	}
	if cfg.APITokenHash != "" || len(cfg.APITokens) != 1 {
		t.Fatalf("config after migration: %+v", cfg)
	}
	if got := cfg.APITokens[0].perms(); got != permAll {
		t.Errorf("a token without scope has %v, want all permissions", got)
	}
	if migrateLegacyAPIToken(cfg) {
		t.Error("migrated twice")
	}

	view := &Config{APITokenHash: strings.Repeat("cd", 32), APITokenScope: "view"}
	migrateLegacyAPIToken(view)
	if got := view.APITokens[0].perms(); got != permView {
		t.Errorf("a view-only token has %v after migration", got)
	}
}

func newAPITestRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api")
	api.Use(apiCrossOriginMiddleware, apiAuthMiddleware())
	api.POST("/rpc", handleAPIRPC)
	api.POST("/storage/upload", requirePerm(permMedia), handleUploadHttp)
	protected := r.Group("/")
	protected.Use(protectedMiddleware())
	protected.GET("/device", func(c *gin.Context) { c.Status(http.StatusOK) })
	return r
}

func doAPIRequest(r *gin.Engine, method, path, body, bearer, cookie string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: "authToken", Value: cookie})
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestAPIAuthMiddleware(t *testing.T) {
	token := setUpAPITest(t)
	r := newAPITestRouter()
	ping := `{"jsonrpc":"2.0","method":"ping","id":1}`

	tests := []struct {
		name, path, bearer, cookie string
		want                       int
	}{
		{"no credentials", "/api/rpc", "", "", http.StatusUnauthorized},
		{"api token", "/api/rpc", token, "", http.StatusOK},
		{"wrong api token", "/api/rpc", apiTokenPrefix + "wrong", "", http.StatusUnauthorized},
		{"login cookie", "/api/rpc", "", "cookie-token", http.StatusOK},
		{"api token on a UI route", "/device", token, "", http.StatusUnauthorized},
		// The token reaches the upload handler, which knows no such upload.
		{"api token for an upload", "/api/storage/upload?uploadId=none", token, "", http.StatusNotFound},
		{"upload without credentials", "/api/storage/upload?uploadId=none", "", "", http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			method := http.MethodPost
			if tt.path == "/device" {
				method = http.MethodGet
			}
			w := doAPIRequest(r, method, tt.path, ping, tt.bearer, tt.cookie)
			if w.Code != tt.want {
				t.Fatalf("status = %d, want %d (%s)", w.Code, tt.want, w.Body.String())
			}
		})
	}
}

func TestAPIRPCDispatch(t *testing.T) {
	token := setUpAPITest(t)
	r := newAPITestRouter()

	call := func(body string) (int, JSONRPCResponse) {
		w := doAPIRequest(r, http.MethodPost, "/api/rpc", body, token, "")
		var resp JSONRPCResponse
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		return w.Code, resp
	}

	if code, resp := call(`{"jsonrpc":"2.0","method":"ping","id":7}`); code != http.StatusOK || resp.Result != "pong" || resp.ID != float64(7) {
		t.Errorf("ping: status %d, response %+v", code, resp)
	}
	for _, method := range []string{"factoryReset", "setDevModeState", "setVideoStreamPaused", "noSuchMethod"} {
		code, resp := call(`{"jsonrpc":"2.0","method":"` + method + `","id":1}`)
		errObj, _ := resp.Error.(map[string]any)
		if code != http.StatusOK || errObj == nil || errObj["code"] != float64(-32601) {
			t.Errorf("%s: status %d, response %+v, want method not found", method, code, resp)
		}
	}
	if code, _ := call(`{not json`); code != http.StatusBadRequest {
		t.Errorf("malformed body: status %d, want 400", code)
	}
}

func TestAPIRejectsCrossSiteRequests(t *testing.T) {
	setUpAPITest(t)
	r := newAPITestRouter()
	// A form or no-cors fetch from another site: text/plain needs no preflight.
	body := `{"jsonrpc":"2.0","method":"setATXPowerAction","params":{"action":"power-long"},"id":1}`

	send := func(site string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/rpc", strings.NewReader(body))
		req.Header.Set("Content-Type", "text/plain")
		req.Header.Set("Sec-Fetch-Site", site)
		req.Header.Set("Origin", "https://evil.example")
		req.AddCookie(&http.Cookie{Name: "authToken", Value: "cookie-token"})
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}
	if code := send("cross-site"); code != http.StatusForbidden {
		t.Errorf("cross-site request: status %d, want 403", code)
	}
	if code := send("same-site"); code != http.StatusForbidden {
		t.Errorf("same-site request from another origin: status %d, want 403", code)
	}
}

func TestAPITokenPermissions(t *testing.T) {
	// Only requests that must be refused are sent: an allowed one would run
	// on the real device.
	tests := []struct {
		perms  []string
		denied []string
	}{
		{[]string{"view"}, []string{"keypressReport", "setJigglerState", "mountWithHTTP", "setATXPowerAction", "wakeHost"}},
		{[]string{"input"}, []string{"mountWithHTTP", "unmountImage", "setATXPowerAction", "sendWOLMagicPacket", "wakeHost"}},
		{[]string{"media"}, []string{"keypressReport", "absMouseReport", "setDCPowerState"}},
		{[]string{"power"}, []string{"keyboardReport", "wheelReport", "mountWithStorage"}},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.perms, "+"), func(t *testing.T) {
			token := setUpAPITestPerms(t, tt.perms...)
			r := newAPITestRouter()
			call := func(method string) JSONRPCResponse {
				w := doAPIRequest(r, http.MethodPost, "/api/rpc", `{"jsonrpc":"2.0","method":"`+method+`","id":1}`, token, "")
				var resp JSONRPCResponse
				_ = json.Unmarshal(w.Body.Bytes(), &resp)
				return resp
			}
			if resp := call("ping"); resp.Result != "pong" {
				t.Errorf("ping: %+v", resp)
			}
			for _, method := range tt.denied {
				resp := call(method)
				if errObj, _ := resp.Error.(map[string]any); errObj == nil || errObj["code"] != float64(-32001) {
					t.Errorf("%s: %+v, want permission denied", method, resp)
				}
			}
		})
	}
}

func TestAPIUploadNeedsMedia(t *testing.T) {
	r := newAPITestRouter()
	for perms, want := range map[string]int{"view": http.StatusForbidden, "input": http.StatusForbidden, "media": http.StatusNotFound} {
		token := setUpAPITestPerms(t, perms)
		if w := doAPIRequest(r, http.MethodPost, "/api/storage/upload?uploadId=none", "", token, ""); w.Code != want {
			t.Errorf("upload with %s: status %d, want %d", perms, w.Code, want)
		}
	}
	// The login cookie keeps all permissions.
	if w := doAPIRequest(r, http.MethodPost, "/api/storage/upload?uploadId=none", "", "", "cookie-token"); w.Code != http.StatusNotFound {
		t.Errorf("upload with the login cookie: status %d, want 404", w.Code)
	}
}

func TestAPIMultipleTokensAndExpiry(t *testing.T) {
	first := setUpAPITestPerms(t, "view")
	second, _ := newAPIToken()
	expired, _ := newAPIToken()
	config.APITokens = append(config.APITokens,
		APIToken{ID: "t2", Name: "second", Hash: hashAPIToken(second), Permissions: []string{"view", "media"}},
		APIToken{ID: "t3", Name: "old", Hash: hashAPIToken(expired), Permissions: []string{"view"}, ExpiresAt: "2020-01-01T00:00:00Z"},
		APIToken{ID: "t4", Name: "broken", Hash: hashAPIToken("jkvm_broken"), Permissions: []string{"view"}, ExpiresAt: "tomorrow"},
	)
	r := newAPITestRouter()
	ping := `{"jsonrpc":"2.0","method":"ping","id":1}`

	for name, tt := range map[string]struct {
		token string
		want  int
	}{
		"first":         {first, http.StatusOK},
		"second":        {second, http.StatusOK},
		"expired":       {expired, http.StatusUnauthorized},
		"unreadable":    {"jkvm_broken", http.StatusUnauthorized},
		"not in a list": {apiTokenPrefix + "nope", http.StatusUnauthorized},
	} {
		if w := doAPIRequest(r, http.MethodPost, "/api/rpc", ping, tt.token, ""); w.Code != tt.want {
			t.Errorf("%s: status %d, want %d", name, w.Code, tt.want)
		}
	}

	// The tokens keep their own permissions.
	if w := doAPIRequest(r, http.MethodPost, "/api/storage/upload?uploadId=none", "", first, ""); w.Code != http.StatusForbidden {
		t.Errorf("upload with the view token: status %d, want 403", w.Code)
	}
	if w := doAPIRequest(r, http.MethodPost, "/api/storage/upload?uploadId=none", "", second, ""); w.Code != http.StatusNotFound {
		t.Errorf("upload with the media token: status %d, want 404", w.Code)
	}

	// The use of a token is remembered for the settings page.
	info := apiTokenInfoOf(config.APITokens[1], time.Now())
	if info.LastUsedAt == "" {
		t.Error("the use of the second token was not noted")
	}
	if got := apiTokenInfoOf(config.APITokens[2], time.Now()); !got.Expired {
		t.Error("the expired token is not shown as expired")
	}
}

func TestAPITokenNeedsHTTPSWhenEnabled(t *testing.T) {
	token := setUpAPITest(t)
	config.TLSMode = "self-signed"
	r := newAPITestRouter()
	w := doAPIRequest(r, http.MethodPost, "/api/rpc", `{"jsonrpc":"2.0","method":"ping","id":1}`, token, "")
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "https") {
		t.Errorf("token over HTTP with HTTPS enabled: status %d, %s", w.Code, w.Body.String())
	}
	req := httptest.NewRequest(http.MethodPost, "/api/rpc", strings.NewReader(`{"jsonrpc":"2.0","method":"ping","id":1}`))
	req.TLS = &tls.ConnectionState{}
	req.Header.Set("Authorization", "Bearer "+token)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("token over HTTPS: status %d, %s", w.Code, w.Body.String())
	}
}
