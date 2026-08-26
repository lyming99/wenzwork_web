package main

import (
	"bytes"
	"encoding/binary"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

const commandTextDetectionLimit = 4 << 10

// CommandTextDecodeResult keeps the raw bytes and their display projection
// together.  Callers persist RawBytes for diagnostics and only expose
// DisplayText after non-interactive VT cleanup.
type CommandTextDecodeResult struct {
	RawBytes        []byte
	DisplayText     string
	SourceEncoding  string
	IsBinary        bool
	HadDecodeErrors bool
}

type commandTextDecoderOptions struct {
	FallbackEncoding string
	SanitizeVT       bool
}

// CommandTextDecoder is a per-stream decoder.  It never shares state between
// stdout and stderr, so a partial UTF-8/UTF-16/GB18030 sequence on one stream
// cannot corrupt the other.
type CommandTextDecoder struct {
	fallbackEncoding string
	sanitize         *VTTextSanitizer

	decided        bool
	sourceEncoding string
	isBinary       bool
	hadErrors      bool
	mode           commandTextMode

	pendingRaw   []byte
	pendingText  strings.Builder
	utf8Pending  []byte
	utf16Little  bool
	utf16Pending []byte
	fallback     transform.Transformer
	fallbackPend []byte

	rawByteCount uint64
}

type commandTextMode uint8

const (
	commandTextUndecided commandTextMode = iota
	commandTextUTF8
	commandTextUTF16
	commandTextFallback
	commandTextBinary
)

func newCommandTextDecoder(options commandTextDecoderOptions) *CommandTextDecoder {
	fallback := strings.ToLower(strings.TrimSpace(options.FallbackEncoding))
	if fallback == "" {
		fallback = defaultCommandTextFallbackEncoding()
	}
	decoder := &CommandTextDecoder{fallbackEncoding: fallback}
	if options.SanitizeVT {
		decoder.sanitize = newVTTextSanitizer()
	}
	return decoder
}

func (decoder *CommandTextDecoder) Feed(raw []byte) []CommandTextDecodeResult {
	if decoder == nil || len(raw) == 0 {
		return nil
	}
	copyOfRaw := append([]byte(nil), raw...)
	decoder.rawByteCount += uint64(len(copyOfRaw))
	decoder.pendingRaw = append(decoder.pendingRaw, copyOfRaw...)
	if !decoder.decided {
		return decoder.decide(false)
	}
	switch decoder.mode {
	case commandTextUTF8:
		decoder.utf8Pending = append(decoder.utf8Pending, copyOfRaw...)
	case commandTextUTF16:
		decoder.utf16Pending = append(decoder.utf16Pending, copyOfRaw...)
	case commandTextFallback:
		decoder.fallbackPend = append(decoder.fallbackPend, copyOfRaw...)
	}
	return decoder.decode(false)
}

func (decoder *CommandTextDecoder) Flush() []CommandTextDecodeResult {
	if decoder == nil {
		return nil
	}
	if !decoder.decided {
		return decoder.decide(true)
	}
	result := decoder.decode(true)
	if decoder.sanitize != nil {
		_ = decoder.sanitize.Flush()
	}
	return result
}

// FlushTruncatedTail finishes a deliberately capped stream without treating a
// partial final code unit as evidence that the entire output is binary. The
// caller still reports truncation; this method only retains the complete text
// that arrived before the raw-byte limit.
func (decoder *CommandTextDecoder) FlushTruncatedTail() []CommandTextDecodeResult {
	if decoder == nil {
		return nil
	}
	if !decoder.decided {
		// A cap can split the very first UTF-8 rune. It is more useful and more
		// accurate to report an empty, truncated UTF-8 projection than to label
		// a known-truncated two-byte prefix as arbitrary binary data.
		if commandOutputHasOnlyIncompleteUTF8(decoder.pendingRaw) {
			decoder.decided = true
			decoder.mode, decoder.sourceEncoding = commandTextUTF8, "utf-8"
			decoder.utf8Pending = append(decoder.utf8Pending, decoder.pendingRaw...)
		} else {
			return decoder.Flush()
		}
	}
	var result []CommandTextDecodeResult
	switch decoder.mode {
	case commandTextUTF8:
		result = decoder.flushTruncatedUTF8()
	case commandTextUTF16:
		result = decoder.flushTruncatedUTF16()
	case commandTextFallback:
		// Any codec tail waiting for more source bytes is deliberately omitted.
		// decodeFallback has already accumulated every complete decoded rune.
		decoder.fallbackPend = decoder.fallbackPend[:0]
		result = decoder.emitText()
	case commandTextBinary:
		result = decoder.emitBinary()
	default:
		decoder.isBinary, decoder.sourceEncoding = true, "binary"
		result = decoder.emitBinary()
	}
	if decoder.sanitize != nil {
		_ = decoder.sanitize.Flush()
	}
	return result
}

func (decoder *CommandTextDecoder) RawByteCount() uint64 {
	if decoder == nil {
		return 0
	}
	return decoder.rawByteCount
}

func (decoder *CommandTextDecoder) decide(atEOF bool) []CommandTextDecodeResult {
	if decoder == nil || decoder.decided {
		return nil
	}
	data := decoder.pendingRaw
	if len(data) == 0 && !atEOF {
		return nil
	}
	if utf8BOMPrefix(data) && len(data) < 3 && !atEOF {
		return nil
	}
	if utf16BOMPrefix(data) && len(data) < 2 && !atEOF {
		return nil
	}
	if (bytes.Equal(data, []byte{0xff, 0xfe}) || bytes.Equal(data, []byte{0xfe, 0xff})) && !atEOF {
		return nil
	}
	decoder.decided = true
	switch {
	case bytes.HasPrefix(data, []byte{0xef, 0xbb, 0xbf}):
		decoder.mode, decoder.sourceEncoding = commandTextUTF8, "utf-8"
		decoder.pendingRaw = decoder.pendingRaw[3:]
		decoder.utf8Pending = append(decoder.utf8Pending, decoder.pendingRaw...)
		decoder.pendingRaw = append([]byte{0xef, 0xbb, 0xbf}, decoder.pendingRaw...)
	case bytes.HasPrefix(data, []byte{0xff, 0xfe}):
		if len(data) == 2 {
			decoder.mode, decoder.sourceEncoding, decoder.isBinary = commandTextBinary, "binary", true
			break
		}
		decoder.mode, decoder.sourceEncoding, decoder.utf16Little = commandTextUTF16, "utf-16le", true
		decoder.utf16Pending = append(decoder.utf16Pending, data[2:]...)
	case bytes.HasPrefix(data, []byte{0xfe, 0xff}):
		if len(data) == 2 {
			decoder.mode, decoder.sourceEncoding, decoder.isBinary = commandTextBinary, "binary", true
			break
		}
		decoder.mode, decoder.sourceEncoding, decoder.utf16Little = commandTextUTF16, "utf-16be", false
		decoder.utf16Pending = append(decoder.utf16Pending, data[2:]...)
	case likelyBinaryCommandOutput(data):
		decoder.mode, decoder.sourceEncoding, decoder.isBinary = commandTextBinary, "binary", true
	case utf8.Valid(data):
		decoder.mode, decoder.sourceEncoding = commandTextUTF8, "utf-8"
		decoder.utf8Pending = append(decoder.utf8Pending, data...)
	case commandOutputHasOnlyIncompleteUTF8(data) && len(data) < commandTextDetectionLimit && !atEOF:
		decoder.decided = false
		return nil
	default:
		transformer, sourceEncoding, ok := commandTextFallbackTransformer(decoder.fallbackEncoding)
		if !ok {
			decoder.mode, decoder.sourceEncoding, decoder.isBinary = commandTextBinary, "binary", true
		} else {
			decoder.mode, decoder.sourceEncoding, decoder.fallback = commandTextFallback, sourceEncoding, transformer
			decoder.fallbackPend = append(decoder.fallbackPend, data...)
		}
	}
	return decoder.decode(atEOF)
}

func utf8BOMPrefix(data []byte) bool {
	bom := []byte{0xef, 0xbb, 0xbf}
	return len(data) > 0 && len(data) < len(bom) && bytes.Equal(data, bom[:len(data)])
}

func utf16BOMPrefix(data []byte) bool {
	return len(data) == 1 && (data[0] == 0xff || data[0] == 0xfe)
}

func commandOutputHasOnlyIncompleteUTF8(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	validEnd, incomplete, invalid := validUTF8Prefix(data)
	return validEnd < len(data) && incomplete && !invalid
}

func (decoder *CommandTextDecoder) decode(atEOF bool) []CommandTextDecodeResult {
	if decoder == nil || !decoder.decided {
		return nil
	}
	if decoder.mode == commandTextBinary || decoder.isBinary {
		return decoder.emitBinary()
	}
	switch decoder.mode {
	case commandTextUTF8:
		return decoder.decodeUTF8(atEOF)
	case commandTextUTF16:
		return decoder.decodeUTF16(atEOF)
	case commandTextFallback:
		return decoder.decodeFallback(atEOF)
	default:
		decoder.isBinary = true
		decoder.sourceEncoding = "binary"
		return decoder.emitBinary()
	}
}

func (decoder *CommandTextDecoder) decodeUTF8(atEOF bool) []CommandTextDecodeResult {
	validEnd, incomplete, invalid := validUTF8Prefix(decoder.utf8Pending)
	if invalid {
		decoder.isBinary, decoder.sourceEncoding = true, "binary"
		return decoder.emitBinary()
	}
	if incomplete {
		if !atEOF {
			return nil
		}
		decoder.isBinary, decoder.sourceEncoding = true, "binary"
		return decoder.emitBinary()
	}
	if validEnd == 0 && len(decoder.pendingRaw) == 0 {
		return nil
	}
	decoder.pendingText.WriteString(string(decoder.utf8Pending[:validEnd]))
	decoder.utf8Pending = decoder.utf8Pending[:0]
	return decoder.emitText()
}

func (decoder *CommandTextDecoder) flushTruncatedUTF8() []CommandTextDecodeResult {
	validEnd, _, invalid := validUTF8Prefix(decoder.utf8Pending)
	if invalid {
		decoder.isBinary, decoder.sourceEncoding = true, "binary"
		return decoder.emitBinary()
	}
	if validEnd > 0 {
		decoder.pendingText.WriteString(string(decoder.utf8Pending[:validEnd]))
	}
	decoder.utf8Pending = decoder.utf8Pending[:0]
	return decoder.emitText()
}

func validUTF8Prefix(data []byte) (validEnd int, incomplete, invalid bool) {
	for validEnd < len(data) {
		runeValue, width := utf8.DecodeRune(data[validEnd:])
		if runeValue == utf8.RuneError && width == 1 {
			if !utf8.FullRune(data[validEnd:]) {
				return validEnd, true, false
			}
			return validEnd, false, true
		}
		validEnd += width
	}
	return validEnd, false, false
}

func (decoder *CommandTextDecoder) decodeUTF16(atEOF bool) []CommandTextDecodeResult {
	if len(decoder.utf16Pending)%2 != 0 {
		if !atEOF {
			return nil
		}
		decoder.isBinary, decoder.sourceEncoding = true, "binary"
		return decoder.emitBinary()
	}
	processable := len(decoder.utf16Pending)
	if !atEOF && processable >= 2 {
		lastOffset := processable - 2
		var last uint16
		if decoder.utf16Little {
			last = binary.LittleEndian.Uint16(decoder.utf16Pending[lastOffset:])
		} else {
			last = binary.BigEndian.Uint16(decoder.utf16Pending[lastOffset:])
		}
		// A high surrogate belongs to the following code unit. Hold it across
		// raw read boundaries instead of emitting a replacement character.
		if last >= 0xd800 && last <= 0xdbff {
			processable -= 2
		}
	}
	if processable == 0 {
		return nil
	}
	words := make([]uint16, 0, processable/2)
	for offset := 0; offset < processable; offset += 2 {
		if decoder.utf16Little {
			words = append(words, binary.LittleEndian.Uint16(decoder.utf16Pending[offset:]))
		} else {
			words = append(words, binary.BigEndian.Uint16(decoder.utf16Pending[offset:]))
		}
	}
	decoded := string(utf16.Decode(words))
	if strings.ContainsRune(decoded, utf8.RuneError) {
		decoder.hadErrors = true
	}
	decoder.utf16Pending = decoder.utf16Pending[processable:]
	decoder.pendingText.WriteString(decoded)
	return decoder.emitText()
}

func (decoder *CommandTextDecoder) flushTruncatedUTF16() []CommandTextDecodeResult {
	processable := len(decoder.utf16Pending) - len(decoder.utf16Pending)%2
	if processable >= 2 {
		lastOffset := processable - 2
		var last uint16
		if decoder.utf16Little {
			last = binary.LittleEndian.Uint16(decoder.utf16Pending[lastOffset:])
		} else {
			last = binary.BigEndian.Uint16(decoder.utf16Pending[lastOffset:])
		}
		if last >= 0xd800 && last <= 0xdbff {
			processable -= 2
		}
	}
	if processable > 0 {
		words := make([]uint16, 0, processable/2)
		for offset := 0; offset < processable; offset += 2 {
			if decoder.utf16Little {
				words = append(words, binary.LittleEndian.Uint16(decoder.utf16Pending[offset:]))
			} else {
				words = append(words, binary.BigEndian.Uint16(decoder.utf16Pending[offset:]))
			}
		}
		decoded := string(utf16.Decode(words))
		if strings.ContainsRune(decoded, utf8.RuneError) {
			decoder.hadErrors = true
		}
		decoder.pendingText.WriteString(decoded)
	}
	decoder.utf16Pending = decoder.utf16Pending[:0]
	return decoder.emitText()
}

func (decoder *CommandTextDecoder) decodeFallback(atEOF bool) []CommandTextDecodeResult {
	if decoder.fallback == nil {
		decoder.isBinary, decoder.sourceEncoding = true, "binary"
		return decoder.emitBinary()
	}
	input := decoder.fallbackPend
	if len(input) == 0 {
		return decoder.emitText()
	}
	var output strings.Builder
	for {
		buffer := make([]byte, max(64, len(input)*4+16))
		nDst, nSrc, err := decoder.fallback.Transform(buffer, input, atEOF)
		if nDst > 0 {
			output.Write(buffer[:nDst])
		}
		input = input[nSrc:]
		switch err {
		case nil:
			decoder.fallbackPend = input
			decoder.pendingText.WriteString(output.String())
			return decoder.emitText()
		case transform.ErrShortSrc:
			decoder.fallbackPend = input
			decoder.pendingText.WriteString(output.String())
			if atEOF {
				decoder.isBinary, decoder.sourceEncoding = true, "binary"
				return decoder.emitBinary()
			}
			return nil
		case transform.ErrShortDst:
			// The destination is sized generously, but retry defensively if a
			// future codec expands more than expected.
			if nSrc == 0 && nDst == 0 {
				decoder.isBinary, decoder.sourceEncoding = true, "binary"
				return decoder.emitBinary()
			}
		default:
			decoder.hadErrors = true
			decoder.isBinary, decoder.sourceEncoding = true, "binary"
			return decoder.emitBinary()
		}
	}
}

func (decoder *CommandTextDecoder) emitText() []CommandTextDecodeResult {
	if decoder == nil || len(decoder.pendingRaw) == 0 {
		return nil
	}
	text := decoder.pendingText.String()
	decoder.pendingText.Reset()
	if decoder.sanitize != nil {
		text = decoder.sanitize.Feed(text)
	}
	result := CommandTextDecodeResult{
		RawBytes: append([]byte(nil), decoder.pendingRaw...), DisplayText: text,
		SourceEncoding: decoder.sourceEncoding, IsBinary: false, HadDecodeErrors: decoder.hadErrors,
	}
	decoder.pendingRaw = decoder.pendingRaw[:0]
	return []CommandTextDecodeResult{result}
}

func (decoder *CommandTextDecoder) emitBinary() []CommandTextDecodeResult {
	if decoder == nil || len(decoder.pendingRaw) == 0 {
		return nil
	}
	decoder.isBinary, decoder.sourceEncoding = true, "binary"
	decoder.pendingText.Reset()
	decoder.utf8Pending = decoder.utf8Pending[:0]
	decoder.utf16Pending = decoder.utf16Pending[:0]
	decoder.fallbackPend = decoder.fallbackPend[:0]
	result := CommandTextDecodeResult{
		RawBytes: append([]byte(nil), decoder.pendingRaw...), SourceEncoding: "binary", IsBinary: true,
		HadDecodeErrors: decoder.hadErrors,
	}
	decoder.pendingRaw = decoder.pendingRaw[:0]
	return []CommandTextDecodeResult{result}
}

func likelyBinaryCommandOutput(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	controls := 0
	for _, value := range data {
		if value == 0 {
			return true
		}
		if value < 0x20 && value != '\n' && value != '\r' && value != '\t' && value != 0x1b {
			controls++
		}
	}
	return controls*8 > len(data)
}

func commandTextFallbackTransformer(name string) (transform.Transformer, string, bool) {
	normalized := strings.ToLower(strings.TrimSpace(name))
	var codec encoding.Encoding
	switch normalized {
	case "gb18030", "windows-acp:54936":
		codec = simplifiedchinese.GB18030
		if normalized == "windows-acp:54936" {
			return codec.NewDecoder(), normalized, true
		}
		return codec.NewDecoder(), "gb18030", true
	case "gbk", "windows-acp:936":
		codec = simplifiedchinese.GBK
		if normalized == "windows-acp:936" {
			return codec.NewDecoder(), normalized, true
		}
		return codec.NewDecoder(), "gbk", true
	case "windows-acp:1252", "windows-1252":
		return charmap.Windows1252.NewDecoder(), "windows-acp:1252", true
	default:
		return nil, "", false
	}
}
