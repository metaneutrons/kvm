package usbgadget

import "testing"

func TestAudioGadgetConfigFollowsEnabledDevice(t *testing.T) {
	u := &UsbGadget{enabledDevices: Devices{Audio: false}}
	if u.isGadgetConfigItemEnabled("audio") {
		t.Fatal("audio gadget should be disabled when audio device is disabled")
	}

	u.enabledDevices.Audio = true
	if !u.isGadgetConfigItemEnabled("audio") {
		t.Fatal("audio gadget should be enabled when audio device is enabled")
	}
}

func TestAudioGadgetDoesNotAllocateMixerStatusEndpoint(t *testing.T) {
	// The RV1106 kernel can leave EP6 IN unusable after a host USB reset. A
	// UAC1 mixer status endpoint pushes mass storage onto that endpoint and
	// makes boot media disappear during installer handover (#1634). #1715
	// reports the same 0.5.9 regression and 0.5.8 workaround.
	for _, attr := range []string{
		"c_mute_present",
		"c_volume_present",
		"p_mute_present",
		"p_volume_present",
	} {
		if got := audioConfig.attrs[attr]; got != "0" {
			t.Errorf("audio %s = %q, want 0 to keep mass storage below EP6 IN", attr, got)
		}
	}
}

func TestBaseGadgetConfigItemsAlwaysEnabled(t *testing.T) {
	u := &UsbGadget{}
	for _, item := range []string{"base", "base_info", "wake_hid"} {
		if !u.isGadgetConfigItemEnabled(item) {
			t.Fatalf("%s should always be enabled", item)
		}
	}
}
