//go:build linux && arm

package kvm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

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

func setUpAPITest(t *testing.T) string {
	t.Helper()
	originalConfig := config
	token, err := newAPIToken()
	if err != nil {
		t.Fatal(err)
	}
	config = &Config{
		LocalAuthMode:  "password",
		LocalAuthToken: "cookie-token",
		APITokenHash:   hashAPIToken(token),
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
	if !isValidAPIToken(token) {
		t.Error("the configured token was rejected")
	}
	for _, bad := range []string{other, "", apiTokenPrefix, strings.TrimPrefix(token, apiTokenPrefix)} {
		if isValidAPIToken(bad) {
			t.Errorf("token %q was accepted", bad)
		}
	}

	config.APITokenHash = ""
	if isValidAPIToken(token) {
		t.Error("a token was accepted while none is configured")
	}
}

func newAPITestRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api")
	api.Use(apiCrossOriginMiddleware, apiAuthMiddleware())
	api.POST("/rpc", handleAPIRPC)
	api.POST("/storage/upload", handleUploadHttp)
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
