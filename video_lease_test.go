//go:build linux && arm

package kvm

import (
	"errors"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/jetkvm/kvm/internal/native"
	"github.com/pion/webrtc/v4"
)

// leaseNative records pipeline calls and keeps the codec, so a restart for a
// codec change is visible.
type leaseNative struct {
	native.EmptyNativeInterface
	codec int
	calls []string
}

func (n *leaseNative) VideoSetCodecType(codecType int) error {
	n.codec = codecType
	n.calls = append(n.calls, "set-codec:"+strconv.Itoa(codecType))
	return nil
}

func (n *leaseNative) VideoGetCodecType() (int, error) { return n.codec, nil }

func (n *leaseNative) VideoStart() error {
	n.calls = append(n.calls, "start")
	return nil
}

func (n *leaseNative) VideoStop() error {
	n.calls = append(n.calls, "stop")
	return nil
}

// setUpLeaseTest isolates the package's global video state for one test.
func setUpLeaseTest(t *testing.T, sessions int) *leaseNative {
	t.Helper()
	rec := &leaseNative{}
	originalNative, originalConfig := nativeInstance, config
	nativeInstance = rec
	config = &Config{VideoSleepAfterSec: -1}

	activeSessionsMutex.Lock()
	originalSessions := actionSessions
	actionSessions = sessions
	activeSessionsMutex.Unlock()
	hostDisplayAdvertiseLock.Lock()
	originalAdvertised := hostDisplayAdvertised
	hostDisplayAdvertised = true
	hostDisplayAdvertiseLock.Unlock()

	reset := func() {
		videoPauseMu.Lock()
		if videoLeaseTimer != nil {
			videoLeaseTimer.Stop()
		}
		videoLeaseTimer = nil
		videoLeaseUntil = time.Time{}
		videoPipelineRunning = false
		videoPausedBy = nil
		videoLeaseActive.Store(false)
		videoPauseMu.Unlock()
	}
	reset()
	t.Cleanup(func() {
		reset()
		hostDisplayAdvertiseLock.Lock()
		hostDisplayAdvertised = originalAdvertised
		hostDisplayAdvertiseLock.Unlock()
		activeSessionsMutex.Lock()
		actionSessions = originalSessions
		activeSessionsMutex.Unlock()
		nativeInstance, config = originalNative, originalConfig
	})
	return rec
}

// expireNow makes the lease due and runs the expiry as the timer would.
func expireNow() {
	videoPauseMu.Lock()
	videoLeaseUntil = time.Now().Add(-time.Second)
	videoPauseMu.Unlock()
	expireVideoLease()
}

func wantCalls(t *testing.T, rec *leaseNative, want ...string) {
	t.Helper()
	if !reflect.DeepEqual(rec.calls, want) {
		t.Fatalf("native calls = %v, want %v", rec.calls, want)
	}
}

func TestVideoLeaseStartsPipelineOnceWithoutSession(t *testing.T) {
	rec := setUpLeaseTest(t, 0)

	if _, err := acquireVideoLease(); err != nil {
		t.Fatal(err)
	}
	if _, err := acquireVideoLease(); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, rec, "set-codec:0", "start")

	expireNow()
	wantCalls(t, rec, "set-codec:0", "start", "stop")
	if videoLeaseActive.Load() {
		t.Fatal("lease still active after expiry")
	}
}

func TestVideoLeaseExpiryWaitsForExtension(t *testing.T) {
	rec := setUpLeaseTest(t, 0)

	if _, err := acquireVideoLease(); err != nil {
		t.Fatal(err)
	}
	// The timer fires while the lease was extended in the meantime.
	expireVideoLease()
	wantCalls(t, rec, "set-codec:0", "start")
	if !videoLeaseActive.Load() {
		t.Fatal("extended lease was released")
	}
}

func TestVideoLeaseRefusesPausedVideoDuringUpload(t *testing.T) {
	rec := setUpLeaseTest(t, 1)
	videoPauseMu.Lock()
	videoPausedBy = &Session{}
	videoPauseMu.Unlock()
	pendingUploadsMutex.Lock()
	pendingUploads["upload_test"] = pendingUpload{}
	pendingUploadsMutex.Unlock()
	t.Cleanup(func() {
		pendingUploadsMutex.Lock()
		delete(pendingUploads, "upload_test")
		pendingUploadsMutex.Unlock()
	})

	if _, err := acquireVideoLease(); !errors.Is(err, errVideoPaused) {
		t.Fatalf("err = %v, want errVideoPaused", err)
	}
	wantCalls(t, rec)
}

func TestVideoLeaseTakesOverPauseOfSession(t *testing.T) {
	rec := setUpLeaseTest(t, 1)
	session := &Session{}
	videoPauseMu.Lock()
	videoPausedBy = session
	videoPauseMu.Unlock()

	if _, err := acquireVideoLease(); err != nil {
		t.Fatalf("err = %v, want the lease to take the pause over", err)
	}
	wantCalls(t, rec, "set-codec:0", "start")
	videoPauseMu.Lock()
	held := videoPausedBy
	videoPauseMu.Unlock()
	if held != nil {
		t.Fatal("the pause is still held")
	}
}

func TestVideoLeaseKeepsPipelineAfterLastSession(t *testing.T) {
	rec := setUpLeaseTest(t, 1)

	startNativeVideoForSession(&Session{codecMimeType: webrtc.MimeTypeH264})
	if _, err := acquireVideoLease(); err != nil {
		t.Fatal(err)
	}
	// The session leaves while the lease is active.
	decrActiveSessions()
	stopNativeVideo()
	wantCalls(t, rec, "set-codec:0", "start")

	expireNow()
	wantCalls(t, rec, "set-codec:0", "start", "stop")
}

func TestVideoLeaseExpiryLeavesSessionPipeline(t *testing.T) {
	rec := setUpLeaseTest(t, 1)

	startNativeVideoForSession(&Session{codecMimeType: webrtc.MimeTypeH264})
	if _, err := acquireVideoLease(); err != nil {
		t.Fatal(err)
	}
	expireNow()
	wantCalls(t, rec, "set-codec:0", "start")
}

func TestSessionRestartsLeasePipelineForOtherCodec(t *testing.T) {
	rec := setUpLeaseTest(t, 0)

	if _, err := acquireVideoLease(); err != nil {
		t.Fatal(err)
	}
	incrActiveSessions()
	startNativeVideoForSession(&Session{codecMimeType: webrtc.MimeTypeH265})
	wantCalls(t, rec, "set-codec:0", "start", "stop", "set-codec:1", "start")

	// Same codec: the running pipeline is reused.
	rec.calls = nil
	startNativeVideoForSession(&Session{codecMimeType: webrtc.MimeTypeH265})
	wantCalls(t, rec)
}

func TestVideoLeaseRestartsPipelineAfterPauseEnded(t *testing.T) {
	rec := setUpLeaseTest(t, 1)
	session := &Session{codecMimeType: webrtc.MimeTypeH264}

	startNativeVideoForSession(session)
	if _, err := acquireVideoLease(); err != nil {
		t.Fatal(err)
	}
	if err := rpcSetVideoStreamPaused(session, true); err != nil {
		t.Fatal(err)
	}
	// The pausing session disconnects: its pause is released, video is not
	// restarted for it, and the lease keeps the stopped pipeline.
	releaseVideoPause(session)
	decrActiveSessions()
	stopNativeVideo()
	wantCalls(t, rec, "set-codec:0", "start", "stop")

	// The next snapshot must bring capture back.
	if _, err := acquireVideoLease(); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, rec, "set-codec:0", "start", "stop", "set-codec:0", "start")
}
