// Package plugin is the host side of goclaw's plugin system. A plugin is a
// black-box compiled binary in plugins/<name>/; the host launches it and speaks a
// length-prefixed binary frame protocol to it over the child's stdin/stdout. The
// host does NOT import the plugin SDK (goclawkit): the wire protocol is a frozen
// contract, so the host carries its own small copy here. This avoids coupling the
// host's build to the SDK, which is what keeps "add a plugin without rebuilding the
// host" true.
//
// This file is the wire protocol; client.go drives one plugin process, and the
// manager (later) discovers and supervises many.
package plugin

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"sync"
)

// Protocol constants. These MUST match goclawkit/pkg/ipc exactly; they are the
// shared wire contract. The header is frozen at ProtocolVer 1; features extend by
// Topic, not by changing these.
const (
	magic       = "GCLW"
	protocolVer = 1
	maxTopicLen = 255
	maxPayload  = 8 * 1024 * 1024 // 8 MiB; reject larger frames
)

// Wire errors.
var (
	errInvalidMagic    = errors.New("plugin: invalid frame magic")
	errUnsupportedVer  = errors.New("plugin: unsupported protocol version")
	errTopicTooLong    = errors.New("plugin: topic too long")
	errPayloadTooLarge = errors.New("plugin: payload too large")
)

// frameType is the small fixed set of message patterns. Features live in the
// topic, not here.
type frameType uint8

const (
	frameControl frameType = 0 // handshake, shutdown, heartbeat: topic names which
	frameRequest frameType = 1 // a request expecting a result (correlated by ID)
	frameResult  frameType = 2 // the reply to a request (same ID)
	frameEvent   frameType = 3 // a one-way push (no reply), e.g. a channel inbound msg
)

// Topics the host uses. These mirror the SDK's (unexported) topic constants.
const (
	topicHello     = "hello"
	topicHelloOK   = "hello.ok"
	topicShutdown  = "shutdown"
	topicHeartbeat = "heartbeat"
	topicInvoke    = "tool.invoke"
)

// frame is one wire message. Payload is opaque bytes (JSON, decoded per topic).
//
// Wire format (all integers big-endian):
//
//	magic "GCLW" (4) | ver(1) | type(1) | flags(1) | id(8) | topicLen(2) |
//	topic(topicLen) | payLen(4) | payload(payLen)
type frame struct {
	Type    frameType
	Flags   uint8
	ID      uint64 // correlates a request to its result; 0 for unsolicited frames
	Topic   string
	Payload []byte
}

// headerLen is the fixed prefix: magic(4)+ver(1)+type(1)+flags(1)+id(8)+topicLen(2).
const headerLen = 17

// writeFrame writes f's header (big-endian) then topic and payload. Callers that
// share a writer must serialize writeFrame themselves (session does this).
func writeFrame(w io.Writer, f frame) error {
	if len(f.Topic) > maxTopicLen {
		return errTopicTooLong
	}
	if len(f.Payload) > maxPayload {
		return errPayloadTooLarge
	}

	var hdr [headerLen]byte
	copy(hdr[0:4], magic)
	hdr[4] = protocolVer
	hdr[5] = byte(f.Type)
	hdr[6] = f.Flags
	binary.BigEndian.PutUint64(hdr[7:15], f.ID)
	binary.BigEndian.PutUint16(hdr[15:17], uint16(len(f.Topic)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := io.WriteString(w, f.Topic); err != nil {
		return err
	}

	var plen [4]byte
	binary.BigEndian.PutUint32(plen[:], uint32(len(f.Payload)))
	if _, err := w.Write(plen[:]); err != nil {
		return err
	}
	if len(f.Payload) > 0 {
		if _, err := w.Write(f.Payload); err != nil {
			return err
		}
	}
	return nil
}

// readFrame reads one frame, verifying magic and version and enforcing the caps.
// It uses io.ReadFull so a frame split across reads is reassembled; every length is
// explicit, so there is no line-length limit.
func readFrame(r io.Reader) (frame, error) {
	var hdr [headerLen]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return frame{}, err
	}
	if string(hdr[0:4]) != magic {
		return frame{}, errInvalidMagic
	}
	if hdr[4] != protocolVer {
		return frame{}, errUnsupportedVer
	}

	f := frame{
		Type:  frameType(hdr[5]),
		Flags: hdr[6],
		ID:    binary.BigEndian.Uint64(hdr[7:15]),
	}
	topicLen := binary.BigEndian.Uint16(hdr[15:17])
	if int(topicLen) > maxTopicLen {
		return frame{}, errTopicTooLong
	}
	if topicLen > 0 {
		topic := make([]byte, topicLen)
		if _, err := io.ReadFull(r, topic); err != nil {
			return frame{}, err
		}
		f.Topic = string(topic)
	}

	var plen [4]byte
	if _, err := io.ReadFull(r, plen[:]); err != nil {
		return frame{}, err
	}
	payLen := binary.BigEndian.Uint32(plen[:])
	if payLen > maxPayload {
		return frame{}, errPayloadTooLarge
	}
	if payLen > 0 {
		payload := make([]byte, payLen)
		if _, err := io.ReadFull(r, payload); err != nil {
			return frame{}, err
		}
		f.Payload = payload
	}
	return f, nil
}

// session wraps a reader/writer pair (the plugin's stdout/stdin from the host's
// view) with a write mutex so concurrent senders never interleave bytes, plus a
// buffered reader for the single read loop.
type session struct {
	w   io.Writer
	r   *bufio.Reader
	wmu sync.Mutex
}

func newSession(r io.Reader, w io.Writer) *session {
	return &session{w: w, r: bufio.NewReader(r)}
}

func (s *session) send(f frame) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	return writeFrame(s.w, f)
}

func (s *session) recv() (frame, error) {
	return readFrame(s.r)
}

// --- payload structs (mirror goclawkit/pkg/plugin, decoded per topic) ---

// hello is the host's handshake payload (frameControl, topic "hello").
type hello struct {
	Magic       string `json:"magic"`
	ProtocolVer int    `json:"protocol_ver"`
}

// helloOK is the plugin's handshake reply (frameControl, topic "hello.ok").
type helloOK struct {
	Magic       string `json:"magic"`
	ProtocolVer int    `json:"protocol_ver"`
	Info        Info   `json:"info"`
}

// invoke is the host's tool-call payload (frameRequest, topic "tool.invoke").
type invoke struct {
	Tool string          `json:"tool"`
	Args json.RawMessage `json:"args"`
}

// result is the plugin's tool-call reply (frameResult, topic "tool.invoke").
type result struct {
	Text    string `json:"text"`
	IsError bool   `json:"is_error"`
}

// Info is what a plugin announces in the handshake (the hello.ok payload). It
// mirrors goclawkit's plugin.Info so the host can read a plugin's identity, kind,
// and advertised tools without invoking anything.
type Info struct {
	Name        string     `json:"name"`
	Kind        string     `json:"kind"`
	Version     string     `json:"version"`
	ProtocolVer int        `json:"protocol_ver"`
	Tools       []ToolInfo `json:"tools,omitempty"`
}

// ToolInfo describes one tool a plugin exposes.
type ToolInfo struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}
