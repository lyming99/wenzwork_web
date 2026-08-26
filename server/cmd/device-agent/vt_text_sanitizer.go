package main

import "strings"

type vtSanitizerState uint8

const (
	vtGround vtSanitizerState = iota
	vtEscape
	vtCSI
	vtOSC
	vtString
	vtStringEscape
)

// VTTextSanitizer removes non-interactive terminal control sequences while
// preserving ordinary text.  It is intentionally stateful because a sequence
// can be split across arbitrary process-read chunks.
type VTTextSanitizer struct {
	state       vtSanitizerState
	pendingCR   bool
	stringKind  string
	removedByID map[string]uint64
}

func newVTTextSanitizer() *VTTextSanitizer {
	return &VTTextSanitizer{removedByID: make(map[string]uint64)}
}

func (sanitizer *VTTextSanitizer) Feed(text string) string {
	if sanitizer == nil || text == "" {
		return ""
	}
	var output strings.Builder
	for _, character := range text {
		sanitizer.consume(&output, character)
	}
	return output.String()
}

func (sanitizer *VTTextSanitizer) Flush() string {
	if sanitizer == nil {
		return ""
	}
	var output strings.Builder
	if sanitizer.pendingCR {
		sanitizer.pendingCR = false
	}
	// Incomplete escape/control strings are intentionally discarded.  They have
	// no printable terminal meaning and must not leak as "^[" into logs.
	if sanitizer.state != vtGround {
		sanitizer.removedByID[sanitizer.sequenceKind()]++
		sanitizer.state = vtGround
		sanitizer.stringKind = ""
	}
	return output.String()
}

func (sanitizer *VTTextSanitizer) RemovedSequenceCounts() map[string]uint64 {
	if sanitizer == nil {
		return nil
	}
	result := make(map[string]uint64, len(sanitizer.removedByID))
	for key, value := range sanitizer.removedByID {
		result[key] = value
	}
	return result
}

func (sanitizer *VTTextSanitizer) consume(output *strings.Builder, character rune) {
	if sanitizer.pendingCR {
		if character == '\n' {
			output.WriteByte('\n')
			sanitizer.pendingCR = false
			return
		}
		output.WriteByte('\n')
		sanitizer.pendingCR = false
	}
	switch sanitizer.state {
	case vtGround:
		sanitizer.consumeGround(output, character)
	case vtEscape:
		sanitizer.consumeEscape(character)
	case vtCSI:
		if character >= 0x40 && character <= 0x7e {
			sanitizer.removedByID["csi"]++
			sanitizer.state = vtGround
		}
	case vtOSC:
		if character == 0x07 {
			sanitizer.removedByID["osc"]++
			sanitizer.state = vtGround
			sanitizer.stringKind = ""
		} else if character == 0x1b {
			sanitizer.state = vtStringEscape
		} else if character == 0x9c {
			sanitizer.removedByID["osc"]++
			sanitizer.state = vtGround
			sanitizer.stringKind = ""
		}
	case vtString:
		if character == 0x1b {
			sanitizer.state = vtStringEscape
		} else if character == 0x9c {
			sanitizer.removedByID[sanitizer.sequenceKind()]++
			sanitizer.state = vtGround
			sanitizer.stringKind = ""
		}
	case vtStringEscape:
		if character == '\\' || character == 0x9c {
			sanitizer.removedByID[sanitizer.sequenceKind()]++
			sanitizer.state = vtGround
			sanitizer.stringKind = ""
			return
		}
		// ESC inside a control string is part of the payload unless it starts ST.
		if sanitizer.stringKind == "osc" {
			sanitizer.state = vtOSC
		} else {
			sanitizer.state = vtString
		}
	}
}

func (sanitizer *VTTextSanitizer) consumeGround(output *strings.Builder, character rune) {
	switch character {
	case 0x1b:
		sanitizer.state = vtEscape
	case 0x9b:
		sanitizer.state = vtCSI
	case 0x9d:
		sanitizer.state, sanitizer.stringKind = vtOSC, "osc"
	case 0x90, 0x98, 0x9e, 0x9f:
		sanitizer.state, sanitizer.stringKind = vtString, "string"
	case '\r':
		sanitizer.pendingCR = true
	case '\n', '\t':
		output.WriteRune(character)
	default:
		if character < 0x20 || (character >= 0x80 && character <= 0x9f) {
			sanitizer.removedByID["control"]++
			return
		}
		output.WriteRune(character)
	}
}

func (sanitizer *VTTextSanitizer) consumeEscape(character rune) {
	switch character {
	case '[':
		sanitizer.state = vtCSI
	case ']':
		sanitizer.state, sanitizer.stringKind = vtOSC, "osc"
	case 'P', '_', '^', 'X':
		sanitizer.state, sanitizer.stringKind = vtString, "string"
	default:
		sanitizer.removedByID["escape"]++
		sanitizer.state = vtGround
	}
}

func (sanitizer *VTTextSanitizer) sequenceKind() string {
	if sanitizer == nil {
		return "string"
	}
	if sanitizer.state == vtCSI {
		return "csi"
	}
	if sanitizer.state == vtEscape {
		return "escape"
	}
	if sanitizer.stringKind != "" {
		return sanitizer.stringKind
	}
	return "string"
}
