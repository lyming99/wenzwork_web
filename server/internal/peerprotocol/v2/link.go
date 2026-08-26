package peerv2

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"
)

var (
	ErrSequence = errors.New("remote/v2 stream sequence is invalid")
	// ErrReplay wraps ErrSequence so existing callers that only classify
	// sequence failures remain compatible while Link handlers can treat a
	// retransmitted, already-authenticated control frame as idempotent.
	ErrReplay         = fmt.Errorf("%w: remote/v2 replayed stream sequence", ErrSequence)
	ErrRekeyConflict  = errors.New("remote/v2 rekey conflicts with current state")
	ErrLinkClosed     = errors.New("remote/v2 link is closed")
	ErrKeyUnavailable = errors.New("remote/v2 key is unavailable")
	ErrStreamClosed   = fmt.Errorf("%w: remote/v2 record targets a closed stream", ErrSequence)
	ErrStreamReuse    = fmt.Errorf("%w: remote/v2 stream ID was reused", ErrSequence)
	ErrSequenceLimit  = fmt.Errorf("%w: remote/v2 sequencer capacity is exhausted", ErrSequence)
)

// GenerateEphemeralKey creates a one-use X25519 key. Its private part must
// remain in memory only and should be cleared with ClearPrivateKey after use.
func GenerateEphemeralKey(randomSource io.Reader) (*ecdh.PrivateKey, error) {
	if randomSource == nil {
		randomSource = rand.Reader
	}
	privateKey, err := ecdh.X25519().GenerateKey(randomSource)
	if err != nil {
		return nil, ErrInvalidHandshake
	}
	return privateKey, nil
}

func X25519SharedSecret(privateKey *ecdh.PrivateKey, peerPublicKey []byte) ([]byte, error) {
	if privateKey == nil || len(peerPublicKey) != X25519PublicKeySize {
		return nil, ErrInvalidHandshake
	}
	publicKey, err := ecdh.X25519().NewPublicKey(peerPublicKey)
	if err != nil {
		return nil, ErrInvalidHandshake
	}
	sharedSecret, err := privateKey.ECDH(publicKey)
	if err != nil || len(sharedSecret) != X25519PublicKeySize {
		return nil, ErrInvalidHandshake
	}
	return sharedSecret, nil
}

// ClearPrivateKey ensures a caller drops its reference as soon as an ECDH
// calculation completes. Go's ecdh.PrivateKey does not offer explicit zeroing,
// so this is a best-effort boundary coupled with no persistent storage.
func ClearPrivateKey(privateKey **ecdh.PrivateKey) {
	if privateKey != nil {
		*privateKey = nil
	}
}

type sequenceKey struct {
	keyID     uint64
	direction Direction
	streamID  string
}

// Sequencer allocates outbound sequences and rejects duplicate inbound
// sequences. Inbound records may arrive out of order during retransmission;
// a bounded sliding window admits them once while rejecting replay.
type Sequencer struct {
	mu             sync.Mutex
	next           map[sequenceKey]uint64
	inbound        map[sequenceKey]*replayWindow
	tombstones     map[sequenceKey]streamTombstone
	activeStreams  map[string]struct{}
	usedStreamIDs  map[string]struct{}
	knownKeys      map[uint64]struct{}
	window         uint64
	tombstoneLimit int
	activeLimit    int
	streamLimit    int
}

const (
	DefaultSequencerTombstoneLimit = 8192
	DefaultSequencerActiveLimit    = 1536
	DefaultSequencerStreamLimit    = 131072
)

type streamTombstone struct {
	maximumSeen uint64
	closedAt    time.Time
}

type SequencerStats struct {
	OutboundEntries       int
	InboundEntries        int
	Tombstones            int
	ActiveStreams         int
	UsedStreamIDs         int
	KeyCount              int
	MaximumClosedSequence uint64
	OldestClosedAt        time.Time
	MaximumStreamsPerLink int
}

func NewSequencer(window uint64) *Sequencer {
	return NewSequencerWithLimits(window, DefaultSequencerTombstoneLimit, DefaultSequencerActiveLimit)
}

func NewSequencerWithLimits(window uint64, tombstoneLimit, activeLimit int) *Sequencer {
	return NewSequencerWithResourceLimits(window, tombstoneLimit, activeLimit, DefaultSequencerStreamLimit)
}

func NewSequencerWithResourceLimits(window uint64, tombstoneLimit, activeLimit, streamLimit int) *Sequencer {
	if window == 0 {
		window = 4096
	}
	if window > 65536 {
		window = 65536
	}
	if tombstoneLimit <= 0 {
		tombstoneLimit = DefaultSequencerTombstoneLimit
	}
	if activeLimit <= 0 {
		activeLimit = DefaultSequencerActiveLimit
	}
	if streamLimit <= 0 {
		streamLimit = DefaultSequencerStreamLimit
	}
	return &Sequencer{
		next: make(map[sequenceKey]uint64), inbound: make(map[sequenceKey]*replayWindow),
		tombstones: make(map[sequenceKey]streamTombstone), activeStreams: make(map[string]struct{}), usedStreamIDs: make(map[string]struct{}),
		knownKeys: make(map[uint64]struct{}), window: window, tombstoneLimit: tombstoneLimit, activeLimit: activeLimit, streamLimit: streamLimit,
	}
}

func (sequencer *Sequencer) OpenStream(streamID string) error {
	if sequencer == nil || !validField(streamID) {
		return ErrSequence
	}
	sequencer.mu.Lock()
	defer sequencer.mu.Unlock()
	if _, active := sequencer.activeStreams[streamID]; active {
		return ErrStreamReuse
	}
	if _, used := sequencer.usedStreamIDs[streamID]; used {
		return ErrStreamReuse
	}
	if len(sequencer.activeStreams) >= sequencer.activeLimit {
		return ErrSequenceLimit
	}
	if len(sequencer.usedStreamIDs) >= sequencer.streamLimit {
		return ErrSequenceLimit
	}
	sequencer.usedStreamIDs[streamID] = struct{}{}
	sequencer.activeStreams[streamID] = struct{}{}
	return nil
}

func (sequencer *Sequencer) CloseStream(streamID string, closedAt time.Time) error {
	if sequencer == nil || !validField(streamID) {
		return ErrSequence
	}
	sequencer.mu.Lock()
	defer sequencer.mu.Unlock()
	if _, used := sequencer.usedStreamIDs[streamID]; !used {
		return ErrSequence
	}
	if _, active := sequencer.activeStreams[streamID]; !active {
		return nil
	}
	keys := make(map[sequenceKey]struct{})
	for keyID := range sequencer.knownKeys {
		keys[sequenceKey{keyID: keyID, direction: DirectionClientToDevice, streamID: streamID}] = struct{}{}
		keys[sequenceKey{keyID: keyID, direction: DirectionDeviceToClient, streamID: streamID}] = struct{}{}
	}
	for key := range sequencer.next {
		if key.streamID == streamID {
			keys[key] = struct{}{}
		}
	}
	for key := range sequencer.inbound {
		if key.streamID == streamID {
			keys[key] = struct{}{}
		}
	}
	additional := 0
	for key := range keys {
		if _, found := sequencer.tombstones[key]; !found {
			additional++
		}
	}
	if len(sequencer.tombstones)+additional > sequencer.tombstoneLimit {
		return ErrSequenceLimit
	}
	if closedAt.IsZero() {
		closedAt = time.Now().UTC()
	} else {
		closedAt = closedAt.UTC()
	}
	for key := range keys {
		maximum := sequencer.next[key]
		if window := sequencer.inbound[key]; window != nil && window.maximum > maximum {
			maximum = window.maximum
		}
		delete(sequencer.next, key)
		delete(sequencer.inbound, key)
		sequencer.tombstones[key] = streamTombstone{maximumSeen: maximum, closedAt: closedAt}
	}
	delete(sequencer.activeStreams, streamID)
	return nil
}

func (sequencer *Sequencer) Stats() SequencerStats {
	if sequencer == nil {
		return SequencerStats{}
	}
	sequencer.mu.Lock()
	defer sequencer.mu.Unlock()
	stats := SequencerStats{
		OutboundEntries: len(sequencer.next), InboundEntries: len(sequencer.inbound), Tombstones: len(sequencer.tombstones),
		ActiveStreams: len(sequencer.activeStreams), UsedStreamIDs: len(sequencer.usedStreamIDs), KeyCount: len(sequencer.knownKeys),
		MaximumStreamsPerLink: sequencer.streamLimit,
	}
	for _, tombstone := range sequencer.tombstones {
		if tombstone.maximumSeen > stats.MaximumClosedSequence {
			stats.MaximumClosedSequence = tombstone.maximumSeen
		}
		if stats.OldestClosedAt.IsZero() || tombstone.closedAt.Before(stats.OldestClosedAt) {
			stats.OldestClosedAt = tombstone.closedAt
		}
	}
	return stats
}

func (sequencer *Sequencer) ShouldRekey(keyID uint64) bool {
	if sequencer == nil || keyID == 0 {
		return false
	}
	sequencer.mu.Lock()
	defer sequencer.mu.Unlock()
	count := 0
	for key := range sequencer.tombstones {
		if key.keyID == keyID {
			count++
		}
	}
	threshold := sequencer.tombstoneLimit / 4
	if threshold < 1 {
		threshold = 1
	}
	return count >= threshold
}

// ShouldRollover asks the Link owner to replace the whole Link before the
// lifetime Stream-ID or tombstone sets reach their fail-closed limits. Rekey
// can retire per-key tombstones, but intentionally cannot make a Stream ID
// reusable within the same Link.
func (sequencer *Sequencer) ShouldRollover() bool {
	if sequencer == nil {
		return false
	}
	sequencer.mu.Lock()
	defer sequencer.mu.Unlock()
	return len(sequencer.usedStreamIDs)*4 >= sequencer.streamLimit*3 ||
		len(sequencer.tombstones)*4 >= sequencer.tombstoneLimit*3
}

func (sequencer *Sequencer) Next(keyID uint64, direction Direction, streamID string) (uint64, error) {
	if sequencer == nil || keyID == 0 || !validDirection(direction) || !validField(streamID) {
		return 0, ErrSequence
	}
	sequencer.mu.Lock()
	defer sequencer.mu.Unlock()
	key := sequenceKey{keyID: keyID, direction: direction, streamID: streamID}
	if _, closed := sequencer.tombstones[key]; closed {
		return 0, ErrStreamClosed
	}
	sequencer.knownKeys[keyID] = struct{}{}
	next := sequencer.next[key] + 1
	if next == 0 {
		return 0, ErrSequence
	}
	sequencer.next[key] = next
	return next, nil
}

func (sequencer *Sequencer) NextOutboundSequence(keyID uint64, direction Direction, streamID string) (uint64, error) {
	if sequencer == nil || keyID == 0 || !validDirection(direction) || !validField(streamID) {
		return 0, ErrSequence
	}
	sequencer.mu.Lock()
	defer sequencer.mu.Unlock()
	key := sequenceKey{keyID: keyID, direction: direction, streamID: streamID}
	if _, closed := sequencer.tombstones[key]; closed {
		return 0, ErrStreamClosed
	}
	next := sequencer.next[key] + 1
	if next == 0 {
		return 0, ErrSequence
	}
	return next, nil
}

func (sequencer *Sequencer) AcceptInbound(metadata RecordMetadata) error {
	if sequencer == nil || !validRecordMetadata(metadata) {
		return ErrSequence
	}
	sequencer.mu.Lock()
	defer sequencer.mu.Unlock()
	key := sequenceKey{keyID: metadata.KeyID, direction: metadata.Direction, streamID: metadata.StreamID}
	if _, closed := sequencer.tombstones[key]; closed {
		return ErrStreamClosed
	}
	sequencer.knownKeys[metadata.KeyID] = struct{}{}
	window := sequencer.inbound[key]
	if window == nil {
		window = newReplayWindow(sequencer.window)
		sequencer.inbound[key] = window
	}
	if !window.accept(metadata.StreamSequence) {
		return ErrReplay
	}
	return nil
}

func (sequencer *Sequencer) RetireKey(keyID uint64) {
	if sequencer == nil || keyID == 0 {
		return
	}
	sequencer.mu.Lock()
	defer sequencer.mu.Unlock()
	for key := range sequencer.next {
		if key.keyID == keyID {
			delete(sequencer.next, key)
		}
	}
	for key := range sequencer.inbound {
		if key.keyID == keyID {
			delete(sequencer.inbound, key)
		}
	}
	for key := range sequencer.tombstones {
		if key.keyID == keyID {
			delete(sequencer.tombstones, key)
		}
	}
	delete(sequencer.knownKeys, keyID)
}

func (sequencer *Sequencer) ResetKey(keyID uint64) {
	sequencer.RetireKey(keyID)
}

func (sequencer *Sequencer) Reset() {
	if sequencer == nil {
		return
	}
	sequencer.mu.Lock()
	defer sequencer.mu.Unlock()
	clear(sequencer.next)
	clear(sequencer.inbound)
	clear(sequencer.tombstones)
	clear(sequencer.activeStreams)
	clear(sequencer.usedStreamIDs)
	clear(sequencer.knownKeys)
}

type replayWindow struct {
	maximum uint64
	width   uint64
	seen    map[uint64]struct{}
}

func newReplayWindow(width uint64) *replayWindow {
	return &replayWindow{width: width, seen: make(map[uint64]struct{})}
}

func (window *replayWindow) accept(sequence uint64) bool {
	if window == nil || sequence == 0 {
		return false
	}
	if _, duplicate := window.seen[sequence]; duplicate {
		return false
	}
	if window.maximum > window.width && sequence <= window.maximum-window.width {
		return false
	}
	window.seen[sequence] = struct{}{}
	if sequence > window.maximum {
		window.maximum = sequence
	}
	minimum := uint64(0)
	if window.maximum > window.width {
		minimum = window.maximum - window.width
	}
	for seen := range window.seen {
		if seen <= minimum {
			delete(window.seen, seen)
		}
	}
	return true
}

// RekeyInit, RekeyAck and RekeyCommit contain plaintext control metadata only
// after their containing frame has been encrypted with the current control
// key. Each identity signature independently binds rekey_id and key_id.
type RekeyInit struct {
	LinkID            string
	RekeyID           string
	NextKeyID         uint64
	EphemeralPublic   []byte
	IdentitySignature []byte
}

type RekeyAck struct {
	LinkID            string
	RekeyID           string
	NextKeyID         uint64
	EphemeralPublic   []byte
	IdentitySignature []byte
}

type StreamBoundary struct {
	StreamID     string
	NextSequence uint64
}

type RekeyCommit struct {
	LinkID     string
	RekeyID    string
	NextKeyID  uint64
	Boundaries []StreamBoundary
}

// RekeyRetry is the bounded, non-secret control state needed to retransmit a
// completed rekey after a Carrier resume. It deliberately contains no private
// key or root material.
type RekeyRetry struct {
	Init      *RekeyInit
	Ack       *RekeyAck
	Commit    *RekeyCommit
	OldKeyID  uint64
	Initiator bool
}

type rekeyPending struct {
	id          string
	nextKeyID   uint64
	initiator   bool
	privateKey  *ecdh.PrivateKey
	publicKey   []byte
	nextRootKey []byte
	init        *RekeyInit
	ack         *RekeyAck
}

func (pending *rekeyPending) initiated() bool { return pending != nil && pending.initiator }

// completedRekey retains only public control metadata for a small bounded
// history. It lets a peer answer a retransmitted ACK/INIT/COMMIT after the
// generation has already activated, without retaining any ephemeral private
// key or root secret.
type completedRekey struct {
	id        string
	nextKeyID uint64
	oldKeyID  uint64
	initiator bool
	init      *RekeyInit
	ack       *RekeyAck
	commit    *RekeyCommit
}

const completedRekeyHistoryLimit = 4

// LinkState keeps at most two active generations: the current key plus the
// previous generation that still has unacknowledged records. It is safe to
// call rekey methods again after a Carrier resume; the same rekey_id returns
// the cached result instead of creating a competing key generation.
type LinkState struct {
	mu             sync.Mutex
	linkID         string
	roots          map[uint64][]byte
	activeKey      uint64
	pending        *rekeyPending
	completed      map[string]*completedRekey
	completedOrder []string
	closed         bool
}

func NewLinkState(linkID string, rootKey []byte) (*LinkState, error) {
	if !validField(linkID) || len(rootKey) != RootKeySize {
		return nil, ErrInvalidHandshake
	}
	return &LinkState{linkID: linkID, roots: map[uint64][]byte{1: append([]byte(nil), rootKey...)}, activeKey: 1, completed: make(map[string]*completedRekey)}, nil
}

func (state *LinkState) completedRekeyLocked(rekeyID string, nextKeyID uint64) *completedRekey {
	if state == nil || rekeyID == "" {
		return nil
	}
	value := state.completed[rekeyID]
	if value == nil || value.nextKeyID != nextKeyID {
		return nil
	}
	return value
}

func (state *LinkState) rememberCompletedLocked(pending *rekeyPending, commit *RekeyCommit) {
	if state == nil || pending == nil || commit == nil {
		return
	}
	if state.completed == nil {
		state.completed = make(map[string]*completedRekey)
	}
	if _, exists := state.completed[pending.id]; exists {
		for index, id := range state.completedOrder {
			if id == pending.id {
				state.completedOrder = append(state.completedOrder[:index], state.completedOrder[index+1:]...)
				break
			}
		}
	}
	state.completed[pending.id] = &completedRekey{
		id: pending.id, nextKeyID: pending.nextKeyID, oldKeyID: pending.nextKeyID - 1, initiator: pending.initiator,
		init: cloneRekeyInit(pending.init), ack: cloneRekeyAck(pending.ack), commit: cloneRekeyCommit(commit),
	}
	state.completedOrder = append(state.completedOrder, pending.id)
	for len(state.completedOrder) > completedRekeyHistoryLimit {
		oldest := state.completedOrder[0]
		state.completedOrder = state.completedOrder[1:]
		delete(state.completed, oldest)
	}
}

func sameRekeyInit(left, right *RekeyInit) bool {
	return left != nil && right != nil && left.LinkID == right.LinkID && left.RekeyID == right.RekeyID && left.NextKeyID == right.NextKeyID && bytes.Equal(left.EphemeralPublic, right.EphemeralPublic) && bytes.Equal(left.IdentitySignature, right.IdentitySignature)
}

func sameRekeyAck(left, right *RekeyAck) bool {
	return left != nil && right != nil && left.LinkID == right.LinkID && left.RekeyID == right.RekeyID && left.NextKeyID == right.NextKeyID && bytes.Equal(left.EphemeralPublic, right.EphemeralPublic) && bytes.Equal(left.IdentitySignature, right.IdentitySignature)
}

func sameRekeyCommit(left, right *RekeyCommit) bool {
	if left == nil || right == nil || left.LinkID != right.LinkID || left.RekeyID != right.RekeyID || left.NextKeyID != right.NextKeyID || !validBoundaries(left.Boundaries) || !validBoundaries(right.Boundaries) {
		return false
	}
	leftBoundaries, rightBoundaries := canonicalBoundaries(left.Boundaries), canonicalBoundaries(right.Boundaries)
	if len(leftBoundaries) != len(rightBoundaries) {
		return false
	}
	for index := range leftBoundaries {
		if leftBoundaries[index] != rightBoundaries[index] {
			return false
		}
	}
	return true
}

func (state *LinkState) LinkID() string {
	if state == nil {
		return ""
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.linkID
}

func (state *LinkState) ActiveKeyID() uint64 {
	if state == nil {
		return 0
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.activeKey
}

func (state *LinkState) RootKey(keyID uint64) ([]byte, error) {
	if state == nil || keyID == 0 {
		return nil, ErrKeyUnavailable
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closed {
		return nil, ErrLinkClosed
	}
	rootKey := state.roots[keyID]
	if len(rootKey) != RootKeySize {
		return nil, ErrKeyUnavailable
	}
	return append([]byte(nil), rootKey...), nil
}

func (state *LinkState) InitiateRekey(identityPrivate ed25519.PrivateKey, randomSource io.Reader) (*RekeyInit, error) {
	if state == nil || len(identityPrivate) != ed25519.PrivateKeySize {
		return nil, ErrInvalidHandshake
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closed {
		return nil, ErrLinkClosed
	}
	if state.pending != nil {
		if state.pending.init != nil {
			return cloneRekeyInit(state.pending.init), nil
		}
		return nil, ErrRekeyConflict
	}
	privateKey, err := GenerateEphemeralKey(randomSource)
	if err != nil {
		return nil, err
	}
	rekeyID, err := randomRekeyID(randomSource)
	if err != nil {
		return nil, err
	}
	nextKeyID := state.activeKey + 1
	publicKey := privateKey.PublicKey().Bytes()
	init := &RekeyInit{LinkID: state.linkID, RekeyID: rekeyID, NextKeyID: nextKeyID, EphemeralPublic: append([]byte(nil), publicKey...)}
	signature, err := signRekeyInit(identityPrivate, *init)
	if err != nil {
		return nil, err
	}
	init.IdentitySignature = signature
	state.pending = &rekeyPending{id: rekeyID, nextKeyID: nextKeyID, initiator: true, privateKey: privateKey, publicKey: append([]byte(nil), publicKey...), init: cloneRekeyInit(init)}
	return cloneRekeyInit(init), nil
}

// ReceiveRekeyInit validates the initiator identity, creates a responder ACK,
// and derives (but does not activate) the next root key.
func (state *LinkState) ReceiveRekeyInit(init RekeyInit, initiatorPublic ed25519.PublicKey, responderPrivate ed25519.PrivateKey, randomSource io.Reader) (*RekeyAck, error) {
	if state == nil || len(initiatorPublic) != ed25519.PublicKeySize || len(responderPrivate) != ed25519.PrivateKeySize {
		return nil, ErrInvalidHandshake
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closed {
		return nil, ErrLinkClosed
	}
	// A completed operation may be replayed after activation. Validate it
	// against the retained transcript before the normal next-generation check.
	if completed := state.completedRekeyLocked(init.RekeyID, init.NextKeyID); completed != nil {
		if completed.init == nil || completed.ack == nil || !sameRekeyInit(completed.init, &init) || validateRekeySignature(state.linkID, init, initiatorPublic) != nil {
			return nil, ErrRekeyConflict
		}
		return cloneRekeyAck(completed.ack), nil
	}
	if err := validateRekeyInit(state.linkID, state.activeKey+1, init, initiatorPublic); err != nil {
		return nil, err
	}
	if state.pending != nil {
		if state.pending.id == init.RekeyID && state.pending.nextKeyID == init.NextKeyID && state.pending.ack != nil && sameRekeyInit(state.pending.init, &init) {
			return cloneRekeyAck(state.pending.ack), nil
		}
		return nil, ErrRekeyConflict
	}
	privateKey, err := GenerateEphemeralKey(randomSource)
	if err != nil {
		return nil, err
	}
	sharedSecret, err := X25519SharedSecret(privateKey, init.EphemeralPublic)
	if err != nil {
		return nil, err
	}
	nextRootKey, err := deriveRekeyRoot(state.roots[state.activeKey], sharedSecret, state.linkID, init.RekeyID, init.NextKeyID)
	wipe(sharedSecret)
	if err != nil {
		return nil, err
	}
	ack := &RekeyAck{LinkID: state.linkID, RekeyID: init.RekeyID, NextKeyID: init.NextKeyID, EphemeralPublic: privateKey.PublicKey().Bytes()}
	signature, err := signRekeyAck(responderPrivate, *ack)
	if err != nil {
		wipe(nextRootKey)
		return nil, err
	}
	ack.IdentitySignature = signature
	state.pending = &rekeyPending{id: init.RekeyID, nextKeyID: init.NextKeyID, initiator: false, privateKey: privateKey, publicKey: append([]byte(nil), ack.EphemeralPublic...), nextRootKey: nextRootKey, init: cloneRekeyInit(&init), ack: cloneRekeyAck(ack)}
	return cloneRekeyAck(ack), nil
}

// ReceiveRekeyAck completes the initiator's ECDH calculation. Repeated ACKs
// for the same operation are harmless and return the existing pending state.
func (state *LinkState) ReceiveRekeyAck(ack RekeyAck, responderPublic ed25519.PublicKey) error {
	if state == nil || len(responderPublic) != ed25519.PublicKeySize {
		return ErrInvalidHandshake
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closed {
		return ErrLinkClosed
	}
	if completed := state.completedRekeyLocked(ack.RekeyID, ack.NextKeyID); completed != nil {
		if completed.ack == nil || !sameRekeyAck(completed.ack, &ack) || validateRekeySignatureAck(state.linkID, ack, responderPublic) != nil {
			return ErrRekeyConflict
		}
		return nil
	}
	if state.pending == nil || state.pending.id != ack.RekeyID || state.pending.nextKeyID != ack.NextKeyID || !state.pending.initiated() {
		return ErrRekeyConflict
	}
	if err := validateRekeyAck(state.linkID, state.activeKey+1, ack, responderPublic); err != nil {
		return err
	}
	if state.pending.ack != nil {
		if !sameRekeyAck(state.pending.ack, &ack) {
			return ErrRekeyConflict
		}
		return nil
	}
	if state.pending.privateKey == nil {
		return ErrRekeyConflict
	}
	sharedSecret, err := X25519SharedSecret(state.pending.privateKey, ack.EphemeralPublic)
	if err != nil {
		return err
	}
	nextRootKey, err := deriveRekeyRoot(state.roots[state.activeKey], sharedSecret, state.linkID, ack.RekeyID, ack.NextKeyID)
	wipe(sharedSecret)
	if err != nil {
		return err
	}
	state.pending.nextRootKey = nextRootKey
	state.pending.ack = cloneRekeyAck(&ack)
	state.pending.privateKey = nil
	return nil
}

func (state *LinkState) CommitRekey(boundaries []StreamBoundary) (*RekeyCommit, error) {
	if state == nil {
		return nil, ErrRekeyConflict
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closed {
		return nil, ErrLinkClosed
	}
	if state.pending == nil {
		// The previous commit may have been sent just before a Carrier failure.
		// Returning its exact bytes makes a retry idempotent and avoids creating
		// a competing generation.
		if len(state.completedOrder) > 0 {
			completed := state.completed[state.completedOrder[len(state.completedOrder)-1]]
			if completed != nil && completed.commit != nil {
				return cloneRekeyCommit(completed.commit), nil
			}
		}
		return nil, ErrRekeyConflict
	}
	if !state.pending.initiated() || len(state.pending.nextRootKey) != RootKeySize || !validBoundaries(boundaries) {
		return nil, ErrRekeyConflict
	}
	commit := &RekeyCommit{LinkID: state.linkID, RekeyID: state.pending.id, NextKeyID: state.pending.nextKeyID, Boundaries: canonicalBoundaries(boundaries)}
	state.activatePendingLocked(commit)
	return commit, nil
}

// LastRekeyRetry returns the newest completed control operation, if any. A
// caller may safely send the returned message again under OldKeyID; repeating
// it is idempotent on the peer.
func (state *LinkState) LastRekeyRetry() (*RekeyRetry, error) {
	if state == nil {
		return nil, ErrRekeyConflict
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closed {
		return nil, ErrLinkClosed
	}
	if len(state.completedOrder) == 0 {
		return nil, ErrKeyUnavailable
	}
	completed := state.completed[state.completedOrder[len(state.completedOrder)-1]]
	if completed == nil {
		return nil, ErrKeyUnavailable
	}
	return &RekeyRetry{
		Init: cloneRekeyInit(completed.init), Ack: cloneRekeyAck(completed.ack), Commit: cloneRekeyCommit(completed.commit),
		OldKeyID: completed.oldKeyID, Initiator: completed.initiator,
	}, nil
}

func (state *LinkState) ReceiveRekeyCommit(commit RekeyCommit) error {
	if state == nil {
		return ErrRekeyConflict
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closed {
		return ErrLinkClosed
	}
	if !validBoundaries(commit.Boundaries) || commit.LinkID != state.linkID {
		return ErrRekeyConflict
	}
	if completed := state.completedRekeyLocked(commit.RekeyID, commit.NextKeyID); completed != nil {
		if completed.commit == nil || !sameRekeyCommit(completed.commit, &commit) {
			return ErrRekeyConflict
		}
		return nil
	}
	if state.pending == nil || len(state.pending.nextRootKey) != RootKeySize || state.pending.initiated() || commit.RekeyID != state.pending.id || commit.NextKeyID != state.pending.nextKeyID {
		return ErrRekeyConflict
	}
	state.activatePendingLocked(&commit)
	return nil
}

// RetireKey removes an old acknowledged generation. The active key is never
// retired, and the memory backing the old root is overwritten first.
func (state *LinkState) RetireKey(keyID uint64) bool {
	if state == nil || keyID == 0 {
		return false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if keyID == state.activeKey {
		return false
	}
	if rootKey := state.roots[keyID]; len(rootKey) > 0 {
		wipe(rootKey)
		delete(state.roots, keyID)
		return true
	}
	return false
}

func (state *LinkState) Close() {
	if state == nil {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closed {
		return
	}
	state.closed = true
	for keyID, rootKey := range state.roots {
		wipe(rootKey)
		delete(state.roots, keyID)
	}
	if state.pending != nil {
		wipe(state.pending.nextRootKey)
		state.pending.privateKey = nil
		state.pending = nil
	}
	for key, completed := range state.completed {
		if completed != nil {
			completed.init = nil
			completed.ack = nil
			completed.commit = nil
		}
		delete(state.completed, key)
	}
	state.completedOrder = nil
}

func (state *LinkState) activatePendingLocked(commit *RekeyCommit) {
	pending := state.pending
	if pending == nil || commit == nil || len(pending.nextRootKey) != RootKeySize {
		return
	}
	state.rememberCompletedLocked(pending, commit)
	state.roots[pending.nextKeyID] = pending.nextRootKey
	state.activeKey = pending.nextKeyID
	pending.nextRootKey = nil
	pending.privateKey = nil
	state.pending = nil
	// A rekey ever has only one active predecessor. This is enough to accept
	// delayed old frames while avoiding unbounded retention after repeated keys.
	for keyID, rootKey := range state.roots {
		if keyID+1 < state.activeKey {
			wipe(rootKey)
			delete(state.roots, keyID)
		}
	}
}

func deriveRekeyRoot(oldRoot, sharedSecret []byte, linkID, rekeyID string, keyID uint64) ([]byte, error) {
	if len(oldRoot) != RootKeySize || len(sharedSecret) != X25519PublicKeySize || !validField(linkID) || !validField(rekeyID) || keyID < 2 {
		return nil, ErrRekeyConflict
	}
	oldDigest := sha256.Sum256(oldRoot)
	info := appendFields(nil, "wenzwork-remote-v2/rekey-root", linkID, rekeyID, uint64Text(keyID))
	return hkdfBytes(sharedSecret, oldDigest[:], info, RootKeySize)
}

func signRekeyInit(privateKey ed25519.PrivateKey, init RekeyInit) ([]byte, error) {
	if len(privateKey) != ed25519.PrivateKeySize || !validRekeyFields(init.LinkID, init.RekeyID, init.NextKeyID, init.EphemeralPublic) {
		return nil, ErrRekeyConflict
	}
	return ed25519.Sign(privateKey, canonicalRekey("init", init.LinkID, init.RekeyID, init.NextKeyID, init.EphemeralPublic)), nil
}

func signRekeyAck(privateKey ed25519.PrivateKey, ack RekeyAck) ([]byte, error) {
	if len(privateKey) != ed25519.PrivateKeySize || !validRekeyFields(ack.LinkID, ack.RekeyID, ack.NextKeyID, ack.EphemeralPublic) {
		return nil, ErrRekeyConflict
	}
	return ed25519.Sign(privateKey, canonicalRekey("ack", ack.LinkID, ack.RekeyID, ack.NextKeyID, ack.EphemeralPublic)), nil
}

func validateRekeyInit(linkID string, expectedKeyID uint64, init RekeyInit, publicKey ed25519.PublicKey) error {
	if init.NextKeyID != expectedKeyID {
		return ErrRekeyConflict
	}
	return validateRekeySignature(linkID, init, publicKey)
}

func validateRekeySignature(linkID string, init RekeyInit, publicKey ed25519.PublicKey) error {
	if !validRekeyFields(init.LinkID, init.RekeyID, init.NextKeyID, init.EphemeralPublic) || init.LinkID != linkID || len(publicKey) != ed25519.PublicKeySize || len(init.IdentitySignature) != Ed25519SignatureSize || !ed25519.Verify(publicKey, canonicalRekey("init", init.LinkID, init.RekeyID, init.NextKeyID, init.EphemeralPublic), init.IdentitySignature) {
		return ErrRekeyConflict
	}
	return nil
}

func validateRekeyAck(linkID string, expectedKeyID uint64, ack RekeyAck, publicKey ed25519.PublicKey) error {
	if ack.NextKeyID != expectedKeyID {
		return ErrRekeyConflict
	}
	return validateRekeySignatureAck(linkID, ack, publicKey)
}

func validateRekeySignatureAck(linkID string, ack RekeyAck, publicKey ed25519.PublicKey) error {
	if !validRekeyFields(ack.LinkID, ack.RekeyID, ack.NextKeyID, ack.EphemeralPublic) || ack.LinkID != linkID || len(publicKey) != ed25519.PublicKeySize || len(ack.IdentitySignature) != Ed25519SignatureSize || !ed25519.Verify(publicKey, canonicalRekey("ack", ack.LinkID, ack.RekeyID, ack.NextKeyID, ack.EphemeralPublic), ack.IdentitySignature) {
		return ErrRekeyConflict
	}
	return nil
}

func validRekeyFields(linkID, rekeyID string, keyID uint64, publicKey []byte) bool {
	return validField(linkID) && validField(rekeyID) && keyID > 1 && len(publicKey) == X25519PublicKeySize
}

func canonicalRekey(kind, linkID, rekeyID string, keyID uint64, publicKey []byte) []byte {
	encoded := appendFields(nil, "wenzwork-remote-v2/rekey", kind, linkID, rekeyID, uint64Text(keyID))
	return appendBytes(encoded, publicKey)
}

func randomRekeyID(randomSource io.Reader) (string, error) {
	if randomSource == nil {
		randomSource = rand.Reader
	}
	value := make([]byte, 18)
	if _, err := io.ReadFull(randomSource, value); err != nil {
		return "", fmt.Errorf("generate rekey id: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func cloneRekeyInit(value *RekeyInit) *RekeyInit {
	if value == nil {
		return nil
	}
	return &RekeyInit{LinkID: value.LinkID, RekeyID: value.RekeyID, NextKeyID: value.NextKeyID, EphemeralPublic: append([]byte(nil), value.EphemeralPublic...), IdentitySignature: append([]byte(nil), value.IdentitySignature...)}
}

func cloneRekeyAck(value *RekeyAck) *RekeyAck {
	if value == nil {
		return nil
	}
	return &RekeyAck{LinkID: value.LinkID, RekeyID: value.RekeyID, NextKeyID: value.NextKeyID, EphemeralPublic: append([]byte(nil), value.EphemeralPublic...), IdentitySignature: append([]byte(nil), value.IdentitySignature...)}
}

func cloneRekeyCommit(value *RekeyCommit) *RekeyCommit {
	if value == nil {
		return nil
	}
	boundaries := make([]StreamBoundary, len(value.Boundaries))
	copy(boundaries, value.Boundaries)
	return &RekeyCommit{LinkID: value.LinkID, RekeyID: value.RekeyID, NextKeyID: value.NextKeyID, Boundaries: boundaries}
}

func canonicalBoundaries(boundaries []StreamBoundary) []StreamBoundary {
	if len(boundaries) == 0 {
		return nil
	}
	result := append([]StreamBoundary(nil), boundaries...)
	sort.Slice(result, func(left, right int) bool { return result[left].StreamID < result[right].StreamID })
	return result
}

func validBoundaries(boundaries []StreamBoundary) bool {
	last := ""
	for _, boundary := range canonicalBoundaries(boundaries) {
		if !validField(boundary.StreamID) || boundary.NextSequence == 0 || boundary.StreamID == last {
			return false
		}
		last = boundary.StreamID
	}
	return true
}
