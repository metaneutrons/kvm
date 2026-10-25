package keyboard

import (
	"errors"
	"reflect"
	"testing"
)

func TestTablesAreComplete(t *testing.T) {
	tbl := loadTables()
	if len(tbl.Layouts) < 19 {
		t.Fatalf("only %d layouts loaded", len(tbl.Layouts))
	}
	if _, ok := FindLayout(DefaultLayout); !ok {
		t.Fatalf("default layout %s is missing", DefaultLayout)
	}
	for _, l := range tbl.Layouts {
		for char, combo := range l.Chars {
			for _, info := range []*KeyInfo{&combo.KeyInfo, combo.AccentKey} {
				if info == nil {
					continue
				}
				if _, ok := tbl.Keys[info.Key]; !ok {
					t.Errorf("%s: %q uses unknown key %s", l.IsoCode, char, info.Key)
				}
			}
		}
	}
}

func key(name string) byte {
	code, _ := KeyCode(name)
	return code
}

func TestStrokes(t *testing.T) {
	plain := func(name string) Stroke { return Stroke{Keys: []byte{key(name)}} }
	shifted := func(name string) Stroke { return Stroke{Modifier: ModifierShiftLeft, Keys: []byte{key(name)}} }
	altGr := func(name string) Stroke { return Stroke{Modifier: ModifierAltRight, Keys: []byte{key(name)}} }

	tests := []struct {
		layout, text string
		want         []Stroke
	}{
		{"en-US", "Hi\r\n", []Stroke{shifted("KeyH"), plain("KeyI"), plain("Enter")}},
		{"en_US", "y", []Stroke{plain("KeyY")}},
		{"de-DE", "yz@", []Stroke{plain("KeyZ"), plain("KeyY"), altGr("KeyQ")}},
		// é is typed as the acute dead key followed by e.
		{"de-DE", "é", []Stroke{plain("Equal"), plain("KeyE")}},
		// The decomposed form is normalized first.
		{"de-DE", "é", []Stroke{plain("Equal"), plain("KeyE")}},
		// A lone dead key needs a space.
		{"de-DE", "^", []Stroke{plain("Backquote"), plain("Space")}},
		// es-ES used a key name that does not exist; npm run keyboard:check catches that now.
		{"es-ES", "õ", []Stroke{altGr("Digit4"), plain("KeyO")}},
		// Entries corrected after comparing the tables with xkb.
		{"de-DE", "<>|", []Stroke{plain("IntlBackslash"), shifted("IntlBackslash"), altGr("IntlBackslash")}},
		{"es-ES", "à", []Stroke{plain("BracketLeft"), plain("KeyA")}},
		{"nl-BE", "wyz", []Stroke{plain("KeyZ"), plain("KeyY"), plain("KeyW")}},
		{"da-DK", "æø", []Stroke{plain("Semicolon"), plain("Quote")}},
		{"hu-HU", "0őű", []Stroke{plain("Backquote"), plain("BracketLeft"), plain("Backslash")}},
		// macOS German: @ is Option+L, and the ISO keys report swapped codes.
		{"de-DE-macos", "@<^", []Stroke{altGr("KeyL"), plain("Backquote"), plain("IntlBackslash"), plain("Space")}},
		{"de-DE-macos", "ñ", []Stroke{altGr("KeyN"), plain("KeyN")}},
	}
	for _, tt := range tests {
		layout, ok := FindLayout(tt.layout)
		if !ok {
			t.Fatalf("layout %s not found", tt.layout)
		}
		got, err := layout.Strokes(tt.text)
		if err != nil {
			t.Errorf("%s %q: %v", tt.layout, tt.text, err)
			continue
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s %q: got %v, want %v", tt.layout, tt.text, got, tt.want)
		}
	}
}

func TestStrokesRejectsUnsupportedChars(t *testing.T) {
	layout, _ := FindLayout("en-US")
	_, err := layout.Strokes("a\tb😀\t")
	var unsupported *UnsupportedError
	if !errors.As(err, &unsupported) {
		t.Fatalf("got %v, want UnsupportedError", err)
	}
	if want := []string{"\t", "😀"}; !reflect.DeepEqual(unsupported.Chars, want) {
		t.Errorf("unsupported chars %q, want %q", unsupported.Chars, want)
	}
}

func TestKeyCodeAndModifierBit(t *testing.T) {
	if code, ok := KeyCode("enter"); !ok || code != 0x28 {
		t.Errorf("KeyCode(enter) = %#x, %v", code, ok)
	}
	if _, ok := KeyCode("NoSuchKey"); ok {
		t.Error("an unknown key name was found")
	}
	if bit, ok := ModifierBit(key("MetaLeft")); !ok || bit != 0x08 {
		t.Errorf("ModifierBit(MetaLeft) = %#x, %v", bit, ok)
	}
	if bit, ok := ModifierBit(key("AltGr")); !ok || bit != ModifierAltRight {
		t.Errorf("ModifierBit(AltGr) = %#x, %v", bit, ok)
	}
	if _, ok := ModifierBit(key("KeyA")); ok {
		t.Error("KeyA was treated as a modifier")
	}
}
