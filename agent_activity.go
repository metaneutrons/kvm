package kvm

import (
	"sync"
	"time"
)

// Input from the API and the MCP server reaches the host while someone may
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
func noteAgentAction(source, action, detail string) {
	event := apiLogger.Info()
	if highFrequencyActions[action] {
		event = apiLogger.Debug()
	}
	event.Str("source", source).Str("action", action).Str("detail", detail).Msg("agent action")

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
		sendAgentActivity(agentActivityEvent{Active: true, Source: source, Action: action})
	}
}

// noteAgentView logs that an agent looked at the screen or read state. It
// sends no input, so the UI is not told.
func noteAgentView(source, action string) {
	apiLogger.Info().Str("source", source).Str("action", action).Msg("agent view")
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
