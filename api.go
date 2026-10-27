package kvm

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// A token carries a set of permissions. Viewing the screen and reading state
// is part of every token; input, virtual media and power control are added
// one by one, so a token only has what its client needs. The login cookie has
// all of them.
type apiPerm uint8

const (
	permView apiPerm = 1 << iota
	permInput
	permMedia
	permPower

	permAll = permView | permInput | permMedia | permPower
)

var apiPermNames = []struct {
	perm apiPerm
	name string
}{
	{permView, "view"},
	{permInput, "input"},
	{permMedia, "media"},
	{permPower, "power"},
}

func (p apiPerm) names() []string {
	var names []string
	for _, n := range apiPermNames {
		if p&n.perm != 0 {
			names = append(names, n.name)
		}
	}
	return names
}

// parseAPIPerms turns permission names into a set. view is always included.
func parseAPIPerms(names []string) (apiPerm, error) {
	perms := permView
	for _, name := range names {
		found := false
		for _, n := range apiPermNames {
			if n.name == name {
				perms |= n.perm
				found = true
			}
		}
		if !found {
			return 0, fmt.Errorf("unknown permission %q", name)
		}
	}
	return perms, nil
}

// apiGrant is what a request's credentials allow.
type apiGrant struct {
	perms   apiPerm
	tokenID string
	name    string
}

type apiGrantKey struct{}

// apiGrantFrom returns the grant of the request's credentials. Without a
// token the request came with the login cookie, which has all permissions.
func apiGrantFrom(ctx context.Context) apiGrant {
	if grant, ok := ctx.Value(apiGrantKey{}).(apiGrant); ok {
		return grant
	}
	return apiGrant{perms: permAll}
}

func apiAllowed(ctx context.Context, perm apiPerm) bool {
	return apiGrantFrom(ctx).perms&perm == perm
}

// requirePerm rejects a request whose token lacks the permission.
func requirePerm(perm apiPerm) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !apiAllowed(c.Request.Context(), perm) {
			sendErrorJsonThenAbort(c, http.StatusForbidden, "This API token lacks the permission for this request")
			return
		}
		c.Next()
	}
}

// The routes under /api are meant for scripts and agents. Besides the login
// cookie they accept an API token as "Authorization: Bearer <token>". Tokens
// are generated on the device, shown once and stored only as a SHA-256 hash;
// without any, bearer authentication is off. A token is valid for /api only,
// not for the UI routes, password changes or WebRTC signaling.

const apiTokenPrefix = "jkvm_"

// apiRPCMethods are the JSON-RPC methods /api/rpc may call, with the
// permission each needs: state queries, input, virtual media and power.
// Device administration (developer mode, SSH keys, TLS, cloud, network,
// updates, factory reset) stays with the UI, so a leaked token cannot take
// over the JetKVM itself. Methods that need a WebRTC session are not listed,
// as an HTTP call has none.
var apiRPCMethods = map[string]apiPerm{
	"ping":                  permView,
	"getDeviceID":           permView,
	"getDeviceCapabilities": permView,
	"getLocalVersion":       permView,
	"getNetworkState":       permView,
	"getVideoState":         permView,
	"getUSBState":           permView,
	"getEDID":               permView,
	"getKeyboardLayout":     permView,
	"getKeyboardLedState":   permView,
	"getKeyDownState":       permView,
	"getJigglerState":       permView,
	"getJigglerConfig":      permView,
	"getVirtualMediaState":  permView,
	"getStorageSpace":       permView,
	"listStorageFiles":      permView,
	"getActiveExtension":    permView,
	"getATXState":           permView,
	"getDCPowerState":       permView,
	"getWakeOnLanDevices":   permView,

	"keyboardReport":  permInput,
	"keypressReport":  permInput,
	"absMouseReport":  permInput,
	"relMouseReport":  permInput,
	"wheelReport":     permInput,
	"setJigglerState": permInput,

	"startStorageFileUpload": permMedia,
	"checkMountUrl":          permMedia,
	"mountWithHTTP":          permMedia,
	"mountWithStorage":       permMedia,
	"rpcMountBuiltInImage":   permMedia,
	"unmountImage":           permMedia,

	"wakeHost":           permPower,
	"setATXPowerAction":  permPower,
	"setDCPowerState":    permPower,
	"sendWOLMagicPacket": permPower,
}

// /api accepts the login cookie as well, and gin parses a text/plain body as
// JSON, so a page on another site could send requests that the browser does
// not preflight. Reject cross-site browser requests; clients that are not
// browsers send no Origin or Sec-Fetch-Site header and pass.
var apiCrossOriginProtection = http.NewCrossOriginProtection()

func apiCrossOriginMiddleware(c *gin.Context) {
	if err := apiCrossOriginProtection.Check(c.Request); err != nil {
		sendErrorJsonThenAbort(c, http.StatusForbidden, "Cross-origin request rejected")
		return
	}
	c.Next()
}

// apiAuthMiddleware accepts an API token as bearer token and otherwise falls
// back to the login cookie. Failed token attempts count against the login
// rate limit.
func apiAuthMiddleware() gin.HandlerFunc {
	cookieAuth := protectedMiddleware()
	return func(c *gin.Context) {
		token, isBearer := strings.CutPrefix(c.GetHeader("Authorization"), "Bearer ")
		if !isBearer {
			cookieAuth(c)
			return
		}

		// With HTTPS enabled, plain HTTP stays open on port 80. Refuse the
		// token there, so a client set up with an http:// URL fails at once
		// instead of sending the token unencrypted on every request.
		if config.TLSMode != "" && c.Request.TLS == nil {
			sendErrorJsonThenAbort(c, http.StatusForbidden, "HTTPS is enabled on this device; use an https:// URL for the API token")
			return
		}

		ip := c.ClientIP()
		if allowed, retryAfter := passwordRateLimiter.IsAllowed(ip); !allowed {
			c.Header("Retry-After", fmt.Sprintf("%d", retryAfter))
			sendErrorJsonThenAbort(c, http.StatusTooManyRequests, "Too many failed attempts. Please try again later.")
			return
		}
		entry, ok := findAPIToken(strings.TrimSpace(token))
		if !ok {
			passwordRateLimiter.RecordFailure(ip)
			sendErrorJsonThenAbort(c, http.StatusUnauthorized, "Unauthorized")
			return
		}
		if entry.expired(time.Now()) {
			sendErrorJsonThenAbort(c, http.StatusUnauthorized, "This API token has expired")
			return
		}
		noteAPITokenUse(entry.ID, ip)
		grant := apiGrant{perms: entry.perms(), tokenID: entry.ID, name: entry.Name}
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), apiGrantKey{}, grant))
		c.Next()
	}
}

// handleAPIRPC answers a single JSON-RPC 2.0 request with the same handlers
// as the WebRTC RPC channel, limited to apiRPCMethods. Errors are reported in
// the JSON-RPC response, so the HTTP status is 200 once the body parsed.
func handleAPIRPC(c *gin.Context) {
	var request JSONRPCRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, JSONRPCResponse{
			JSONRPC: "2.0",
			Error:   map[string]any{"code": -32700, "message": "Parse error"},
		})
		return
	}

	handler, ok := rpcHandlers[request.Method]
	perm, listed := apiRPCMethods[request.Method]
	if !ok || !listed {
		c.JSON(http.StatusOK, JSONRPCResponse{
			JSONRPC: "2.0",
			Error:   map[string]any{"code": -32601, "message": "Method not found"},
			ID:      request.ID,
		})
		return
	}

	ctx := c.Request.Context()
	if !apiAllowed(ctx, perm) {
		c.JSON(http.StatusOK, JSONRPCResponse{
			JSONRPC: "2.0",
			Error:   map[string]any{"code": -32001, "message": "This API token lacks the permission for this method"},
			ID:      request.ID,
		})
		return
	}
	if perm != permView {
		noteAgentAction(ctx, "rpc", request.Method, "")
	}

	scopedLogger := jsonRpcLogger.With().
		Str("method", request.Method).
		Str("source", "api").
		Interface("id", request.ID).Logger()

	result, err := callRPCHandler(scopedLogger, handler, request.Params, nil)
	if err != nil {
		scopedLogger.Error().Err(err).Msg("Error calling RPC handler")
		c.JSON(http.StatusOK, JSONRPCResponse{
			JSONRPC: "2.0",
			Error:   map[string]any{"code": -32603, "message": "Internal error", "data": err.Error()},
			ID:      request.ID,
		})
		return
	}

	c.JSON(http.StatusOK, JSONRPCResponse{JSONRPC: "2.0", Result: result, ID: request.ID})
}
