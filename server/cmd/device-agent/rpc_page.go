package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
)

// rpcPagePrefixLength returns the largest non-empty prefix whose complete
// encoded response fits the preferred page budget. The builder must include
// every piece of response metadata (cursor, watermark, flags, and so on), so
// JSON escaping and cursor overhead are accounted for instead of estimated
// from the raw item bodies.
func rpcPagePrefixLength(itemCount int, build func(int) any) (int, error) {
	if build == nil {
		return 0, errRPCInvalid
	}
	return rpcPagePrefixLengthE(itemCount, func(count int) (any, error) { return build(count), nil })
}

func rpcPagePrefixLengthE(itemCount int, build func(int) (any, error)) (int, error) {
	if itemCount < 0 || build == nil {
		return 0, errRPCInvalid
	}
	for count := itemCount; count >= 0; count-- {
		page, err := build(count)
		if err != nil {
			return 0, err
		}
		encoded, err := json.Marshal(page)
		if err != nil {
			return 0, err
		}
		if len(encoded) <= preferredRPCPagePayload {
			if count == 0 && itemCount > 0 {
				// Advancing a cursor without returning the first atomic item would
				// either skip data or create an infinite empty-page loop.
				return 0, errRPCResponsePageTooLarge
			}
			return count, nil
		}
	}
	return 0, errRPCResponsePageTooLarge
}

// rpcPageSnapshotWatermark binds an offset cursor to both its resource and its
// exact ordered snapshot. It is content-free on the wire: only the first
// 64 bits of SHA-256 are carried in the opaque cursor.
func rpcPageSnapshotWatermark(value any) (uint64, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return 0, err
	}
	digest := sha256.Sum256(encoded)
	return safeRPCPageWatermark(binary.BigEndian.Uint64(digest[:8])), nil
}

// safeRPCPageWatermark keeps cursor values portable across JSON clients.
//
// Page watermarks are emitted both as JSON numbers and inside opaque cursors.
// The complete SHA-256 prefix is a uint64, but JavaScript and several native
// JSON decoders only preserve integer precision through 2^53 - 1.
func safeRPCPageWatermark(value uint64) uint64 {
	watermark := value & maxSafeJSONInteger
	if watermark == 0 {
		watermark = 1
	}
	return watermark
}
