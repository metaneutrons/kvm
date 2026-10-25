package kvm

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// The routes under /api are meant for scripts and agents. Besides the login
// cookie they accept an API token as "Authorization: Bearer <token>". The
// token is generated on the device, shown once and stored only as a SHA-256
// hash; without one, bearer authentication is off. It is valid for /api only,
// not for the UI routes, password changes or WebRTC signaling.

const apiTokenPrefix = "jkvm_"

// apiRPCMethods are the JSON-RPC methods /api/rpc may call: state queries,
// input, virtual media and power. Device administration (developer mode, SSH
// keys, TLS, cloud, network, updates, factory reset) stays with the UI, so a
// leaked token cannot take over the JetKVM itself. Methods that need a
// WebRTC session are not listed, as an HTTP call has none.
var apiRPCMethods = map[string]bool{
	"ping":                  true,
	"getDeviceID":           true,
	"getDeviceCapabilities": true,
	"getLocalVersion":       true,
	"getNetworkState":       true,
	"getVideoState":         true,
	"getUSBState":           true,
	"getEDID":               true,
	"getKeyboardLayout":     true,
	"getKeyboardLedState":   true,
	"getKeyDownState":       true,
	"keyboardReport":        true,
	"keypressReport":        true,
	"absMouseReport":        true,
	"relMouseReport":        true,
	"wheelReport":           true,
	"wakeHost":              true,
	"getJigglerState":       true,
	"getVirtualMediaState":  true,
	"getStorageSpace":       true,
	"listStorageFiles":      true,
	"checkMountUrl":         true,
	"mountWithHTTP":         true,
	"mountWithStorage":      true,
	"rpcMountBuiltInImage":  true,
	"unmountImage":          true,
	"getActiveExtension":    true,
	"getATXState":           true,
	"setATXPowerAction":     true,
	"getDCPowerState":       true,
	"setDCPowerState":       true,
	"getWakeOnLanDevices":   true,
	"sendWOLMagicPacket":    true,
}

func hashAPIToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func newAPIToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return apiTokenPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// isValidAPIToken compares in constant time; the hashes have a fixed length.
func isValidAPIToken(token string) bool {
	if config.APITokenHash == "" || !strings.HasPrefix(token, apiTokenPrefix) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(hashAPIToken(token)), []byte(config.APITokenHash)) == 1
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

		ip := c.ClientIP()
		if allowed, retryAfter := passwordRateLimiter.IsAllowed(ip); !allowed {
			c.Header("Retry-After", fmt.Sprintf("%d", retryAfter))
			sendErrorJsonThenAbort(c, http.StatusTooManyRequests, "Too many failed attempts. Please try again later.")
			return
		}
		if !isValidAPIToken(strings.TrimSpace(token)) {
			passwordRateLimiter.RecordFailure(ip)
			sendErrorJsonThenAbort(c, http.StatusUnauthorized, "Unauthorized")
			return
		}
		c.Next()
	}
}

type apiTokenState struct {
	Enabled   bool   `json:"enabled"`
	CreatedAt string `json:"created_at,omitempty"`
}

func handleGetAPIToken(c *gin.Context) {
	c.JSON(http.StatusOK, apiTokenState{
		Enabled:   config.APITokenHash != "",
		CreatedAt: config.APITokenCreatedAt,
	})
}

// setAPIToken stores the token hash and keeps the previous token if the
// configuration cannot be saved, so the response matches what is in effect.
func setAPIToken(hash, createdAt string) error {
	prevHash, prevCreatedAt := config.APITokenHash, config.APITokenCreatedAt
	config.APITokenHash, config.APITokenCreatedAt = hash, createdAt
	if err := SaveConfig(); err != nil {
		config.APITokenHash, config.APITokenCreatedAt = prevHash, prevCreatedAt
		return err
	}
	return nil
}

// handleCreateAPIToken creates a new token, replacing an existing one. The
// token is only returned here and cannot be read back later.
func handleCreateAPIToken(c *gin.Context) {
	token, err := newAPIToken()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate token"})
		return
	}

	if err := setAPIToken(hashAPIToken(token), time.Now().UTC().Format(time.RFC3339)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save configuration"})
		return
	}

	logger.Info().Str("ip", c.ClientIP()).Msg("API token created")
	c.JSON(http.StatusOK, gin.H{"token": token, "created_at": config.APITokenCreatedAt})
}

func handleDeleteAPIToken(c *gin.Context) {
	if err := setAPIToken("", ""); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save configuration"})
		return
	}

	logger.Info().Str("ip", c.ClientIP()).Msg("API token deleted")
	c.JSON(http.StatusOK, gin.H{"message": "API token deleted"})
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
	if !ok || !apiRPCMethods[request.Method] {
		c.JSON(http.StatusOK, JSONRPCResponse{
			JSONRPC: "2.0",
			Error:   map[string]any{"code": -32601, "message": "Method not found"},
			ID:      request.ID,
		})
		return
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
