package kvm

import (
	"context"
	"sync"
	"time"
)

// Input from the API reaches the host while someone may
// be watching in the web UI. Every action is logged, and the UI is told while
// an agent is active, so a person at the device can see where input comes
// from.

const (
	// agentActivityHold is how long the UI keeps showing activity after the
	// last action.
	agentActivityHold = 10 * time.Second
	// agentActivityRefresh limits the events sent while actions continue.
	agentActivityRefresh = 2 * time.Second
)

type agentActivityEvent struct {
	Active bool   `json:"active"`
	Source string `json:"source,omitempty"`
	Action string `json:"action,omitempty"`
	// Token names the API token that was used. It is empty for the login
	// cookie and when the credentials are not known.
	Token string `json:"token,omitempty"`
}

var agentActivity struct {
	mu        sync.Mutex
	active    bool
	lastEvent time.Time
	timer     *time.Timer
}

// highFrequencyActions are logged at debug level: a script moving the mouse
// sends them many times per second.
var highFrequencyActions = map[string]bool{
	"absMouseReport": true,
	"relMouseReport": true,
	"wheelReport":    true,
	"keyboardReport": true,
	"keypressReport": true,
}

// noteAgentAction logs an action taken through the API and tells the UI that
// an agent is active. detail must not contain typed text.
func noteAgentAction(ctx context.Context, source, action, detail string) {
	token := apiTokenName(ctx)
	event := apiLogger.Info()
	if highFrequencyActions[action] {
		event = apiLogger.Debug()
	}
	event.Str("source", source).Str("action", action).Str("detail", detail).Str("token", token).Msg("agent action")

	agentActivity.mu.Lock()
	now := time.Now()
	notify := !agentActivity.active || now.Sub(agentActivity.lastEvent) >= agentActivityRefresh
	agentActivity.active = true
	if notify {
		agentActivity.lastEvent = now
	}
	if agentActivity.timer != nil {
		agentActivity.timer.Stop()
	}
	agentActivity.timer = time.AfterFunc(agentActivityHold, endAgentActivity)
	agentActivity.mu.Unlock()

	if notify {
		sendAgentActivity(agentActivityEvent{Active: true, Source: source, Action: action, Token: token})
	}
}

// apiTokenName returns the name of the token behind a request, or "" for the
// login cookie.
func apiTokenName(ctx context.Context) string {
	if grant, ok := ctx.Value(apiGrantKey{}).(apiGrant); ok {
		return grant.name
	}
	return ""
}

func sendAgentActivity(event agentActivityEvent) {
	if session := currentSession; session != nil {
		writeJSONRPCEvent("agentActivity", event, session)
	}
}

func endAgentActivity() {
	agentActivity.mu.Lock()
	agentActivity.active = false
	agentActivity.timer = nil
	agentActivity.mu.Unlock()
	sendAgentActivity(agentActivityEvent{Active: false})
}
