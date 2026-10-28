package kvm

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
)

const (
	apiTokenLimit   = 16
	apiTokenNameMax = 40
	apiTokenMaxDays = 3650
)

// APIToken is a token as stored in the configuration. Only its hash is kept.
type APIToken struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Hash        string   `json:"hash"`
	Permissions []string `json:"permissions"`
	CreatedAt   string   `json:"created_at"`
	ExpiresAt   string   `json:"expires_at,omitempty"`
}

func (t APIToken) perms() apiPerm {
	perms, err := parseAPIPerms(t.Permissions)
	if err != nil {
		// A configuration that was edited by hand grants nothing.
		return 0
	}
	return perms
}

// expired fails closed: a token with an unreadable expiry does not work.
func (t APIToken) expired(now time.Time) bool {
	if t.ExpiresAt == "" {
		return false
	}
	expires, err := time.Parse(time.RFC3339, t.ExpiresAt)
	return err != nil || !now.Before(expires)
}

// apiTokensLock guards config.APITokens, which the auth middleware reads on
// every request.
var apiTokensLock sync.RWMutex

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

// findAPIToken looks a token up by its hash. It compares every stored hash in
// constant time; the hashes have a fixed length.
func findAPIToken(token string) (APIToken, bool) {
	if !strings.HasPrefix(token, apiTokenPrefix) {
		return APIToken{}, false
	}
	hash := []byte(hashAPIToken(token))

	apiTokensLock.RLock()
	defer apiTokensLock.RUnlock()
	var found APIToken
	ok := false
	for _, t := range config.APITokens {
		if subtle.ConstantTimeCompare(hash, []byte(t.Hash)) == 1 {
			found, ok = t, true
		}
	}
	return found, ok
}

// The last use of a token is kept in memory only: writing it to the flash on
// every request would wear it out.
type apiTokenUsage struct {
	at time.Time
	ip string
}

var (
	apiTokenUsageLock sync.Mutex
	apiTokenUsed      = map[string]apiTokenUsage{}
)

func noteAPITokenUse(id, ip string) {
	apiTokenUsageLock.Lock()
	apiTokenUsed[id] = apiTokenUsage{at: time.Now(), ip: ip}
	apiTokenUsageLock.Unlock()
}

func forgetAPITokenUse(id string) {
	apiTokenUsageLock.Lock()
	delete(apiTokenUsed, id)
	apiTokenUsageLock.Unlock()
}

// apiTokenInputError is a problem with the request, as opposed to the device.
type apiTokenInputError string

func (e apiTokenInputError) Error() string { return string(e) }

// newAPITokenRecord validates the request and generates a token. It returns
// the token itself, which is not stored, and the record to store.
func newAPITokenRecord(name string, permNames []string, days int, now time.Time) (string, APIToken, error) {
	name = strings.TrimSpace(name)
	if n := utf8.RuneCountInString(name); n == 0 || n > apiTokenNameMax {
		return "", APIToken{}, apiTokenInputError("name must have 1 to 40 characters")
	}
	perms, err := parseAPIPerms(permNames)
	if err != nil {
		return "", APIToken{}, apiTokenInputError(err.Error())
	}
	if days < 0 || days > apiTokenMaxDays {
		return "", APIToken{}, apiTokenInputError("expires_in_days must be from 0 (never) to 3650")
	}

	token, err := newAPIToken()
	if err != nil {
		return "", APIToken{}, err
	}
	idBytes := make([]byte, 6)
	if _, err := rand.Read(idBytes); err != nil {
		return "", APIToken{}, err
	}
	record := APIToken{
		ID:          hex.EncodeToString(idBytes),
		Name:        name,
		Hash:        hashAPIToken(token),
		Permissions: perms.names(),
		CreatedAt:   now.UTC().Format(time.RFC3339),
	}
	if days > 0 {
		record.ExpiresAt = now.UTC().AddDate(0, 0, days).Format(time.RFC3339)
	}
	return token, record, nil
}

// migrateLegacyAPIToken turns the single token of earlier versions into an
// entry of the token list. It reports whether it changed anything.
func migrateLegacyAPIToken(cfg *Config) bool {
	if cfg.APITokenHash == "" {
		return false
	}
	if !slices.ContainsFunc(cfg.APITokens, func(t APIToken) bool { return t.Hash == cfg.APITokenHash }) {
		perms := permAll
		if cfg.APITokenScope == "view" {
			perms = permView
		}
		id := cfg.APITokenHash
		if len(id) > 12 {
			id = id[:12]
		}
		cfg.APITokens = append(cfg.APITokens, APIToken{
			ID:          id,
			Name:        "API token",
			Hash:        cfg.APITokenHash,
			Permissions: perms.names(),
			CreatedAt:   cfg.APITokenCreatedAt,
		})
	}
	cfg.APITokenHash, cfg.APITokenCreatedAt, cfg.APITokenScope = "", "", ""
	return true
}

var errAPITokenNotFound = errors.New("no such API token")

// updateAPITokens changes the token list and saves it. If the configuration
// cannot be saved, the previous list stays in effect, so the response matches
// what is in effect.
func updateAPITokens(change func([]APIToken) ([]APIToken, error)) error {
	apiTokensLock.Lock()
	defer apiTokensLock.Unlock()

	prev := config.APITokens
	next, err := change(slices.Clone(prev))
	if err != nil {
		return err
	}
	config.APITokens = next
	if err := SaveConfig(); err != nil {
		config.APITokens = prev
		return err
	}
	return nil
}

type apiTokenInfo struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Permissions []string `json:"permissions"`
	CreatedAt   string   `json:"created_at"`
	ExpiresAt   string   `json:"expires_at,omitempty"`
	Expired     bool     `json:"expired,omitempty"`
	LastUsedAt  string   `json:"last_used_at,omitempty"`
	LastUsedIP  string   `json:"last_used_ip,omitempty"`
}

func apiTokenInfoOf(t APIToken, now time.Time) apiTokenInfo {
	info := apiTokenInfo{
		ID:          t.ID,
		Name:        t.Name,
		Permissions: t.perms().names(),
		CreatedAt:   t.CreatedAt,
		ExpiresAt:   t.ExpiresAt,
		Expired:     t.expired(now),
	}
	apiTokenUsageLock.Lock()
	use, ok := apiTokenUsed[t.ID]
	apiTokenUsageLock.Unlock()
	if ok {
		info.LastUsedAt = use.at.UTC().Format(time.RFC3339)
		info.LastUsedIP = use.ip
	}
	return info
}

func handleListAPITokens(c *gin.Context) {
	apiTokensLock.RLock()
	tokens := slices.Clone(config.APITokens)
	apiTokensLock.RUnlock()

	now := time.Now()
	infos := make([]apiTokenInfo, 0, len(tokens))
	for _, t := range tokens {
		infos = append(infos, apiTokenInfoOf(t, now))
	}
	c.JSON(http.StatusOK, gin.H{"tokens": infos, "limit": apiTokenLimit})
}

// handleCreateAPIToken adds a token. The token itself is only returned here
// and cannot be read back later.
func handleCreateAPIToken(c *gin.Context) {
	var req struct {
		Name          string   `json:"name"`
		Permissions   []string `json:"permissions"`
		ExpiresInDays int      `json:"expires_in_days"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	now := time.Now()
	token, record, err := newAPITokenRecord(req.Name, req.Permissions, req.ExpiresInDays, now)
	if err != nil {
		respondAPITokenError(c, err)
		return
	}
	err = updateAPITokens(func(tokens []APIToken) ([]APIToken, error) {
		if len(tokens) >= apiTokenLimit {
			return nil, apiTokenInputError("too many API tokens, delete one first")
		}
		return append(tokens, record), nil
	})
	if err != nil {
		respondAPITokenError(c, err)
		return
	}

	logger.Info().Str("ip", c.ClientIP()).Str("token", record.Name).
		Strs("permissions", record.Permissions).Msg("API token created")
	c.JSON(http.StatusOK, gin.H{"token": token, "info": apiTokenInfoOf(record, now)})
}

func handleDeleteAPIToken(c *gin.Context) {
	id := c.Param("id")
	var name string
	err := updateAPITokens(func(tokens []APIToken) ([]APIToken, error) {
		i := slices.IndexFunc(tokens, func(t APIToken) bool { return t.ID == id })
		if i < 0 {
			return nil, errAPITokenNotFound
		}
		name = tokens[i].Name
		return slices.Delete(tokens, i, i+1), nil
	})
	if err != nil {
		respondAPITokenError(c, err)
		return
	}

	forgetAPITokenUse(id)
	logger.Info().Str("ip", c.ClientIP()).Str("token", name).Msg("API token deleted")
	c.JSON(http.StatusOK, gin.H{"message": "API token deleted"})
}

func respondAPITokenError(c *gin.Context, err error) {
	var input apiTokenInputError
	switch {
	case errors.As(err, &input):
		c.JSON(http.StatusBadRequest, gin.H{"error": input.Error()})
	case errors.Is(err, errAPITokenNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save configuration"})
	}
}
