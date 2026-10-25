// Package keyboard turns text and key names into USB HID keyboard reports.
//
// The tables in layouts.json are exported from the UI (ui/src/keyboardLayouts)
// with `npm run keyboard:export`, so the device types text the same way as the
// paste dialog. Do not edit the JSON by hand.
package keyboard

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"golang.org/x/text/unicode/norm"
)

//go:embed layouts.json
var layoutsJSON []byte

// DefaultLayout is used when a layout is not known, as in the UI.
const DefaultLayout = "en-US"

const (
	ModifierShiftLeft = 0x02
	ModifierAltRight  = 0x40
)

type KeyInfo struct {
	Key      string `json:"key"`
	Shift    bool   `json:"shift,omitempty"`
	AltRight bool   `json:"altRight,omitempty"`
}

type KeyCombo struct {
	KeyInfo
	DeadKey   bool     `json:"deadKey,omitempty"`
	AccentKey *KeyInfo `json:"accentKey,omitempty"`
}

type Layout struct {
	IsoCode string              `json:"isoCode"`
	Name    string              `json:"name"`
	Chars   map[string]KeyCombo `json:"chars"`
}

// Stroke is one key press: the modifiers and keys are pressed together and
// then released.
type Stroke struct {
	Modifier byte
	Keys     []byte
}

type tables struct {
	Keys    map[string]byte `json:"keys"`
	Layouts []Layout        `json:"layouts"`
}

var loadTables = sync.OnceValue(func() *tables {
	var t tables
	if err := json.Unmarshal(layoutsJSON, &t); err != nil {
		panic(fmt.Sprintf("keyboard: invalid layouts.json: %v", err))
	}
	return &t
})

// KeyCode returns the HID usage ID of a key name such as "KeyA" or "Enter",
// matched case-insensitively.
func KeyCode(name string) (byte, bool) {
	keys := loadTables().Keys
	if code, ok := keys[name]; ok {
		return code, true
	}
	for n, code := range keys {
		if strings.EqualFold(n, name) {
			return code, true
		}
	}
	return 0, false
}

// ModifierBit returns the bit of a modifier key (HID usage 0xE0 to 0xE7) in
// the report's modifier byte.
func ModifierBit(code byte) (byte, bool) {
	if code < 0xe0 || code > 0xe7 {
		return 0, false
	}
	return 1 << (code - 0xe0), true
}

// LayoutCodes returns the ISO codes of all layouts.
func LayoutCodes() []string {
	layouts := loadTables().Layouts
	codes := make([]string, 0, len(layouts))
	for _, l := range layouts {
		codes = append(codes, l.IsoCode)
	}
	sort.Strings(codes)
	return codes
}

// FindLayout returns the layout with the given ISO code. Like the UI, it
// accepts the legacy "en_US" spelling.
func FindLayout(isoCode string) (*Layout, bool) {
	isoCode = strings.ReplaceAll(isoCode, "_", "-")
	layouts := loadTables().Layouts
	for i := range layouts {
		if strings.EqualFold(layouts[i].IsoCode, isoCode) {
			return &layouts[i], true
		}
	}
	return nil, false
}

func (l *Layout) stroke(info KeyInfo) Stroke {
	var modifier byte
	if info.Shift {
		modifier |= ModifierShiftLeft
	}
	if info.AltRight {
		modifier |= ModifierAltRight
	}
	// The export checks that every key name exists.
	code := loadTables().Keys[info.Key]
	return Stroke{Modifier: modifier, Keys: []byte{code}}
}

// UnsupportedError lists the characters a layout cannot type.
type UnsupportedError struct {
	Layout string
	Chars  []string
}

func (e *UnsupportedError) Error() string {
	quoted := make([]string, len(e.Chars))
	for i, c := range e.Chars {
		quoted[i] = fmt.Sprintf("%q", c)
	}
	return fmt.Sprintf("layout %s cannot type %s", e.Layout, strings.Join(quoted, ", "))
}

// Strokes returns the key strokes that type text on a host using this layout.
// It fails without typing anything if a character is not in the layout.
func (l *Layout) Strokes(text string) ([]Stroke, error) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = norm.NFC.String(text)

	var strokes []Stroke
	var unsupported []string
	seen := map[string]bool{}
	for _, r := range text {
		char := string(r)
		combo, ok := l.Chars[char]
		if !ok {
			if !seen[char] {
				seen[char] = true
				unsupported = append(unsupported, char)
			}
			continue
		}
		// An accented character is typed as the dead accent key followed by
		// the base key.
		if combo.AccentKey != nil {
			strokes = append(strokes, l.stroke(*combo.AccentKey))
		}
		strokes = append(strokes, l.stroke(combo.KeyInfo))
		// A dead key on its own needs a space to produce the accent itself.
		if combo.DeadKey {
			strokes = append(strokes, l.stroke(KeyInfo{Key: "Space"}))
		}
	}
	if len(unsupported) > 0 {
		return nil, &UnsupportedError{Layout: l.IsoCode, Chars: unsupported}
	}
	return strokes, nil
}
