package input

/*
#cgo LDFLAGS: -framework ApplicationServices -framework Carbon
#include <ApplicationServices/ApplicationServices.h>
#include <Carbon/Carbon.h>
#include <unistd.h>

// Every event carries explicit modifier flags. Events otherwise inherit the
// system's modifier state, so one modifier macOS wrongly believes is still held
// turns every click into a ⌘-click and every typed letter into a shortcut.
static const CGEventFlags noModifiers = kCGEventFlagMaskNonCoalesced;

static const CGKeyCode modifierCodes[4] = { kVK_Command, kVK_Shift, kVK_Option, kVK_Control };
static const CGEventFlags modifierMasks[4] = {
	kCGEventFlagMaskCommand, kCGEventFlagMaskShift, kCGEventFlagMaskAlternate, kCGEventFlagMaskControl,
};

static void postEvent(CGEventRef event, CGEventFlags flags) {
	CGEventSetFlags(event, flags);
	CGEventPost(kCGHIDEventTap, event);
	CFRelease(event);
}

// A modifier press or release has to be a flags-changed event; posted as a plain
// key up, macOS never sees the modifier come back up.
static void postModifier(CGEventSourceRef source, CGKeyCode code, bool down, CGEventFlags flags) {
	CGEventRef event = CGEventCreateKeyboardEvent(source, code, down);
	CGEventSetType(event, kCGEventFlagsChanged);
	postEvent(event, flags);
}

static void tapKey(CGKeyCode code, int modifiers) {
	CGEventSourceRef source = CGEventSourceCreate(kCGEventSourceStateHIDSystemState);
	CGEventFlags flags = noModifiers;
	for (int i = 0; i < 4; i++) {
		if (modifiers & (1 << i)) {
			flags |= modifierMasks[i];
			postModifier(source, modifierCodes[i], true, flags);
			usleep(2000);
		}
	}
	postEvent(CGEventCreateKeyboardEvent(source, code, true), flags);
	usleep(3000);
	postEvent(CGEventCreateKeyboardEvent(source, code, false), flags);
	for (int i = 3; i >= 0; i--) {
		if (modifiers & (1 << i)) {
			usleep(2000);
			flags &= ~modifierMasks[i];
			postModifier(source, modifierCodes[i], false, flags);
		}
	}
	CFRelease(source);
}

static void releaseModifiers(void) {
	CGEventSourceRef source = CGEventSourceCreate(kCGEventSourceStateHIDSystemState);
	for (int i = 0; i < 4; i++) {
		postModifier(source, modifierCodes[i], false, noModifiers);
	}
	CFRelease(source);
}

static void typeUnits(const UniChar *units, int count) {
	CGEventSourceRef source = CGEventSourceCreate(kCGEventSourceStateHIDSystemState);
	CGEventRef down = CGEventCreateKeyboardEvent(source, 0, true);
	CGEventKeyboardSetUnicodeString(down, count, units);
	postEvent(down, noModifiers);
	usleep(2000);
	CGEventRef up = CGEventCreateKeyboardEvent(source, 0, false);
	CGEventKeyboardSetUnicodeString(up, count, units);
	postEvent(up, noModifiers);
	CFRelease(source);
}

static void mouseButton(int button, bool down, int clickState) {
	CGEventRef here = CGEventCreate(NULL);
	CGPoint point = CGEventGetLocation(here);
	CFRelease(here);

	CGEventType type;
	CGMouseButton cgButton;
	switch (button) {
	case 1:
		type = down ? kCGEventRightMouseDown : kCGEventRightMouseUp;
		cgButton = kCGMouseButtonRight;
		break;
	case 2:
		type = down ? kCGEventOtherMouseDown : kCGEventOtherMouseUp;
		cgButton = kCGMouseButtonCenter;
		break;
	default:
		type = down ? kCGEventLeftMouseDown : kCGEventLeftMouseUp;
		cgButton = kCGMouseButtonLeft;
	}
	CGEventSourceRef source = CGEventSourceCreate(kCGEventSourceStateHIDSystemState);
	CGEventRef event = CGEventCreateMouseEvent(source, type, point, cgButton);
	CGEventSetIntegerValueField(event, kCGMouseEventClickState, clickState);
	postEvent(event, noModifiers);
	CFRelease(source);
}

static void scrollWheel(int dx, int dy) {
	CGEventSourceRef source = CGEventSourceCreate(kCGEventSourceStateHIDSystemState);
	postEvent(CGEventCreateScrollWheelEvent(source, kCGScrollEventUnitPixel, 2, dy, dx), noModifiers);
	CFRelease(source);
}

// keyCodeForChar finds the key that types ch on the current keyboard layout
// without modifiers, or returns -1.
static int keyCodeForChar(UniChar ch) {
	TISInputSourceRef layoutSource = TISCopyCurrentKeyboardLayoutInputSource();
	CFDataRef data = layoutSource ? TISGetInputSourceProperty(layoutSource, kTISPropertyUnicodeKeyLayoutData) : NULL;
	if (data == NULL) {
		if (layoutSource) CFRelease(layoutSource);
		layoutSource = TISCopyCurrentASCIICapableKeyboardLayoutInputSource();
		data = layoutSource ? TISGetInputSourceProperty(layoutSource, kTISPropertyUnicodeKeyLayoutData) : NULL;
	}
	if (data == NULL) {
		if (layoutSource) CFRelease(layoutSource);
		return -1;
	}
	const UCKeyboardLayout *layout = (const UCKeyboardLayout *)CFDataGetBytePtr(data);
	int found = -1;
	for (UInt16 code = 0; code < 128 && found < 0; code++) {
		UInt32 deadKeys = 0;
		UniChar out[4];
		UniCharCount length = 0;
		OSStatus status = UCKeyTranslate(layout, code, kUCKeyActionDisplay, 0, LMGetKbdType(),
			kUCKeyTranslateNoDeadKeysBit, &deadKeys, 4, &length, out);
		if (status == noErr && length == 1 && out[0] == ch) {
			found = code;
		}
	}
	CFRelease(layoutSource);
	return found;
}
*/
import "C"

import (
	"strings"
	"time"
	"unicode"
	"unicode/utf16"
)

// Bits of the modifiers argument to tapKey, in the order of modifierCodes.
const (
	modCommand = 1 << iota
	modShift
	modOption
	modControl
)

var modifierNames = map[string]int{
	"cmd":     modCommand,
	"command": modCommand,
	"meta":    modCommand,
	"shift":   modShift,
	"alt":     modOption,
	"option":  modOption,
	"opt":     modOption,
	"ctrl":    modControl,
	"control": modControl,
}

// Virtual key codes of the named keys the viewer sends (Carbon's kVK_* values).
var namedKeys = map[string]C.CGKeyCode{
	"enter": 36, "return": 36, "tab": 48, "space": 49, "backspace": 51,
	"escape": 53, "esc": 53, "delete": 117,
	"left": 123, "right": 124, "down": 125, "up": 126,
	"home": 115, "end": 119, "pageup": 116, "pagedown": 121,
	"f1": 122, "f2": 120, "f3": 99, "f4": 118, "f5": 96, "f6": 97,
	"f7": 98, "f8": 100, "f9": 101, "f10": 109, "f11": 103, "f12": 111,
}

// keyCode resolves a key name or single character, reporting whether it needs
// shift added (an uppercase letter).
func keyCode(key string) (code C.CGKeyCode, shift bool, ok bool) {
	if code, ok := namedKeys[strings.ToLower(key)]; ok {
		return code, false, true
	}
	runes := []rune(key)
	if len(runes) != 1 || runes[0] > 0xFFFF {
		return 0, false, false
	}
	if found := C.keyCodeForChar(C.UniChar(runes[0])); found >= 0 {
		return C.CGKeyCode(found), false, true
	}
	if lower := unicode.ToLower(runes[0]); lower != runes[0] {
		if found := C.keyCodeForChar(C.UniChar(lower)); found >= 0 {
			return C.CGKeyCode(found), true, true
		}
	}
	return 0, false, false
}

// pressKey taps key with mods held, reporting false when key is not one it knows.
func pressKey(key string, mods []string) bool {
	code, shift, ok := keyCode(key)
	if !ok {
		return false
	}
	modifiers := 0
	if shift {
		modifiers |= modShift
	}
	for _, m := range mods {
		modifiers |= modifierNames[strings.ToLower(m)]
	}
	C.tapKey(code, C.int(modifiers))
	return true
}

// releaseModifiers tells macOS every modifier is up, clearing one left stuck.
func releaseModifiers() {
	C.releaseModifiers()
}

func typeText(text string) {
	for _, r := range text {
		units := utf16.Encode([]rune{r})
		C.typeUnits((*C.UniChar)(&units[0]), C.int(len(units)))
		time.Sleep(time.Millisecond)
	}
}

func mouseButton(button string, down bool, clickState int) {
	C.mouseButton(C.int(buttonIndex(button)), C.bool(down), C.int(clickState))
}

func click(button string) {
	mouseButton(button, true, 1)
	time.Sleep(5 * time.Millisecond)
	mouseButton(button, false, 1)
}

// doubleClick sends both clicks; the second carries click state 2, which is
// what makes macOS treat the pair as a double click.
func doubleClick(button string) {
	for state := 1; state <= 2; state++ {
		mouseButton(button, true, state)
		time.Sleep(5 * time.Millisecond)
		mouseButton(button, false, state)
		time.Sleep(5 * time.Millisecond)
	}
}

func scroll(dx, dy int) {
	C.scrollWheel(C.int(dx), C.int(dy))
}

func buttonIndex(b string) int {
	switch b {
	case "right":
		return 1
	case "center", "middle":
		return 2
	default:
		return 0
	}
}
