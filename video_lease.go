package kvm

import (
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/jetkvm/kvm/internal/native"
)

// The video pipeline normally runs only while a WebRTC session is connected.
// A video lease keeps it running for consumers without a session, such as
// snapshots. Each use extends the lease by videoLeaseHold; when it expires and
// no session is connected, the pipeline stops and the HDMI sleep timer starts
// again. Every start and stop of the pipeline, for sessions and leases alike,
// happens under videoPauseMu and updates videoPipelineRunning.

const (
	// videoLeaseHold keeps the pipeline running after the last use, so an agent
	// taking screenshots in a loop does not restart capture every time.
	videoLeaseHold = 30 * time.Second
	// videoDisplayWakeTimeout bounds the wait for the host to send a picture
	// after its display was advertised again (host_display_disable_when_idle).
	videoDisplayWakeTimeout = 10 * time.Second
	// videoStartTimeout bounds the wait for the first encoded frame after the
	// lease started the pipeline.
	videoStartTimeout = 3 * time.Second
)

var (
	errVideoPaused = errors.New("video is paused while a virtual media upload runs; try again when it has finished")

	// videoLeaseActive is readable without videoPauseMu for the display
	// advertisement and the sleep timer, which hold other locks.
	videoLeaseActive atomic.Bool

	// guarded by videoPauseMu
	videoPipelineRunning bool
	videoLeaseUntil      time.Time
	videoLeaseTimer      *time.Timer
)

// errNoVideoSignal reports the capture state when the HDMI input has no picture.
type errNoVideoSignal struct{ reason string }

func (e errNoVideoSignal) Error() string {
	return fmt.Sprintf("no video signal: %s", e.reason)
}

// startVideoPipelineLocked starts capture with the given codec, restarting it
// if it already runs with another one. The caller holds videoPauseMu.
func startVideoPipelineLocked(codecType int) {
	if videoPipelineRunning {
		if current, err := nativeInstance.VideoGetCodecType(); err == nil && current == codecType {
			return
		}
		_ = nativeInstance.VideoStop()
	}
	_ = nativeInstance.VideoSetCodecType(codecType)
	_ = nativeInstance.VideoStart()
	videoPipelineRunning = true
}

// stopVideoPipelineLocked stops capture. The caller holds videoPauseMu.
func stopVideoPipelineLocked() error {
	videoPipelineRunning = false
	return nativeInstance.VideoStop()
}

// markVideoPipelineStopped records that a restarted native process came up
// without capture, so the next session or lease starts it again.
func markVideoPipelineStopped() {
	videoPauseMu.Lock()
	defer videoPauseMu.Unlock()
	videoPipelineRunning = false
}

// acquireVideoLease extends the lease and starts the pipeline if nothing runs
// it. A pause that a web session holds, for example while its media dialog is
// open, does not keep the lease out: the agent takes it over, as an open
// browser tab must not block it. Only a running upload, which needs the
// memory the pipeline would use, makes it wait. It reports whether the host display was advertised again, in which case
// the host needs a few seconds before it sends a picture.
func acquireVideoLease() (displayWoken bool, err error) {
	videoPauseMu.Lock()
	defer videoPauseMu.Unlock()

	if videoPausedBy != nil {
		if uploadInProgress() {
			return false, errVideoPaused
		}
		nativeLogger.Info().Msg("video lease takes over a pause held by a session")
		videoPausedBy = nil
	}

	videoLeaseUntil = time.Now().Add(videoLeaseHold)
	if videoLeaseTimer == nil {
		videoLeaseTimer = time.AfterFunc(videoLeaseHold, expireVideoLease)
	}
	videoLeaseActive.Store(true)

	if videoPipelineRunning {
		return false, nil
	}

	nativeLogger.Info().Msg("starting video for a lease")
	stopVideoSleepModeTicker()
	wasAdvertised := isHostDisplayAdvertised()
	_ = applyHostDisplayAdvertisement("video_lease")
	codecType := 0
	if currentSession != nil {
		codecType = sessionVideoCodecType(currentSession)
	}
	startVideoPipelineLocked(codecType)
	return !wasAdvertised && isHostDisplayAdvertised(), nil
}

// expireVideoLease runs when the lease timer fires.
func expireVideoLease() {
	videoPauseMu.Lock()
	if remaining := time.Until(videoLeaseUntil); remaining > 0 {
		// extended since the timer was armed
		videoLeaseTimer.Reset(remaining)
		videoPauseMu.Unlock()
		return
	}
	videoLeaseTimer = nil
	videoLeaseActive.Store(false)
	stopped := false
	if videoPipelineRunning && getActiveSessions() == 0 {
		nativeLogger.Info().Msg("video lease expired, stopping video")
		_ = stopVideoPipelineLocked()
		stopped = true
	}
	videoPauseMu.Unlock()

	if stopped {
		// Both read the session count themselves, so a session that connected
		// since the stop is handled correctly.
		_ = applyHostDisplayAdvertisement("video_lease_expired")
		startVideoSleepModeTicker()
	}
}

// takeVideoSnapshot encodes the current HDMI frame as JPEG, starting capture
// first if no session runs it.
func takeVideoSnapshot(opts native.SnapshotOptions) ([]byte, error) {
	displayWoken, err := acquireVideoLease()
	if err != nil {
		return nil, err
	}

	deadline := time.Now().Add(videoStartTimeout)
	if displayWoken {
		deadline = time.Now().Add(videoDisplayWakeTimeout)
	}

	for {
		jpeg, err := nativeInstance.VideoSnapshot(opts)
		if !errors.Is(err, native.ErrVideoNotStreaming) {
			return jpeg, err
		}

		// The format detection runs independently of the pipeline, so its state
		// is current unless the host was just asked to turn its display on.
		state := lastVideoState
		noSignal := !state.Ready && state.Error != ""
		if noSignal && !displayWoken {
			return nil, errNoVideoSignal{reason: state.Error}
		}
		if time.Now().After(deadline) {
			if noSignal {
				return nil, errNoVideoSignal{reason: state.Error}
			}
			return nil, err
		}
		time.Sleep(100 * time.Millisecond)
	}
}
