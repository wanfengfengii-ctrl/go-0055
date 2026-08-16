// Package ipp implements a restricted, strictly-validating IPP 2.x binary
// codec and an IPP-over-HTTP client covering the six operations the
// PressGuard spooler needs (Get-Printer-Attributes, Create-Job,
// Send-Document, Release-Job, Cancel-Job, Get-Job-Attributes) plus
// Get-Jobs, which is required to recover a printer-job-id from a stable
// job-name when a Create-Job response is lost.
//
// The codec is deliberately defensive: it rejects illegal tags, duplicate
// scalar attributes, duplicate attribute groups, attributes appearing
// before any group delimiter, truncated messages, mismatched
// request-ids, unknown status codes and oversized fields. Every failure
// is reported as a typed ProtoError so callers can record a stable
// error category on the attempt rather than retrying blindly.
package ipp

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"unicode/utf8"
)

// IPP version 2.0.
const Version20 uint16 = 0x0200

// Operations.
const (
	OpGetPrinterAttributes uint16 = 0x000B
	OpCreateJob            uint16 = 0x0005
	OpSendDocument         uint16 = 0x0006
	OpReleaseJob           uint16 = 0x000D
	OpCancelJob            uint16 = 0x0008
	OpGetJobAttributes     uint16 = 0x0009
	OpGetJobs              uint16 = 0x000A
)

// Group delimiter tags.
const (
	TagOperations   uint8 = 0x01
	TagJob          uint8 = 0x02
	TagEnd          uint8 = 0x03
	TagPrinter      uint8 = 0x04
	TagUnsupported  uint8 = 0x05
	TagSubscription uint8 = 0x06
)

// Value tags.
const (
	TagUnsupportedVal uint8 = 0x10
	TagUnknown        uint8 = 0x13
	TagInteger        uint8 = 0x21
	TagBoolean        uint8 = 0x22
	TagEnum           uint8 = 0x23
	TagOctetString    uint8 = 0x30
	TagDateTime       uint8 = 0x31
	TagResolution     uint8 = 0x32
	TagRange          uint8 = 0x33
	TagText           uint8 = 0x34
	TagName           uint8 = 0x35
	TagKeyword        uint8 = 0x41
	TagURI            uint8 = 0x42
	TagURIScheme      uint8 = 0x43
	TagCharset        uint8 = 0x44
	TagNaturalLang    uint8 = 0x45
	TagMime           uint8 = 0x46
)

// IPP job-state enum values.
const (
	JobStatePending        uint32 = 3
	JobStatePendingHeld    uint32 = 4
	JobStateProcessing     uint32 = 5
	JobStateProcessingStop uint32 = 6
	JobStateCanceled       uint32 = 7
	JobStateAborted        uint32 = 8
	JobStateCompleted      uint32 = 9
)

// IPP finishings enum values, per RFC 8011. The finishings attribute is a
// multi-valued enum where 3 ("none") denotes the absence of finishing and is
// therefore not a binding capability. Only the base finishing options the
// domain model recognises are listed here; other values (cover,
// edge-stitch, fold, the directional staple variants 20..31, etc.) are
// intentionally omitted and treated as unknown by finishingName.
const (
	FinishingsNone         uint32 = 3
	FinishingsStaple       uint32 = 4
	FinishingsPunch        uint32 = 5
	FinishingsBind         uint32 = 7
	FinishingsSaddleStitch uint32 = 8
)

// IPP status codes.
const (
	StatusOK             uint16 = 0x0000
	StatusOKIgnored      uint16 = 0x0001
	StatusOKConflicting  uint16 = 0x0002
	StatusClientError    uint16 = 0x0400
	StatusServerError     uint16 = 0x0500
)

// isGroupTag reports whether b is a valid attribute-group delimiter.
func isGroupTag(b uint8) bool {
	switch b {
	case TagOperations, TagJob, TagPrinter, TagUnsupported, TagSubscription:
		return true
	}
	return false
}

// isValueTag reports whether b is a known value tag. Unknown value tags
// are rejected as protocol errors during decode.
func isValueTag(b uint8) bool {
	switch b {
	case TagUnsupportedVal, TagUnknown, TagInteger, TagBoolean, TagEnum,
		TagOctetString, TagDateTime, TagResolution, TagRange, TagText,
		TagName, TagKeyword, TagURI, TagURIScheme, TagCharset, TagNaturalLang, TagMime:
		return true
	}
	return false
}

// ErrKind classifies a protocol-level failure so callers can record a
// stable error category and decide whether to retry.
type ErrKind string

const (
	ErrKindIllegalTag       ErrKind = "illegal_tag"
	ErrKindDuplicateScalar  ErrKind = "duplicate_scalar"
	ErrKindDuplicateGroup   ErrKind = "duplicate_group"
	ErrKindAttrWithoutGroup ErrKind = "attribute_without_group"
	ErrKindTruncated        ErrKind = "truncated"
	ErrKindRequestID        ErrKind = "request_id_mismatch"
	ErrKindUnknownStatus    ErrKind = "unknown_status"
	ErrKindOversized        ErrKind = "oversized"
	ErrKindBadUTF8          ErrKind = "bad_utf8"
	ErrKindHTTP             ErrKind = "http"
	ErrKindIPPStatus        ErrKind = "ipp_status"
	ErrKindNetwork          ErrKind = "network"
	ErrKindTimeout          ErrKind = "timeout"
	ErrKindCanceled         ErrKind = "canceled"
)

// ProtoError is a typed protocol/network error. Its Kind is stable and
// machine-readable; Msg carries diagnostics.
type ProtoError struct {
	Kind ErrKind
	Msg  string
}

// Error implements error.
func (e *ProtoError) Error() string {
	return fmt.Sprintf("ipp:%s: %s", e.Kind, e.Msg)
}

// protoErrf is a small constructor.
func protoErrf(k ErrKind, format string, args ...any) *ProtoError {
	return &ProtoError{Kind: k, Msg: fmt.Sprintf(format, args...)}
}

// Attr is one attribute value. For multi-valued attributes the first
// value carries the Name and subsequent values carry an empty Name
// (additional-value encoding).
type Attr struct {
	Tag   uint8
	Name  string
	Value []byte
}

// Group is a tagged attribute group.
type Group struct {
	Tag   uint8
	Attrs []Attr
}

// Add appends a single-valued attribute to the group.
func (g *Group) Add(tag uint8, name string, val []byte) {
	g.Attrs = append(g.Attrs, Attr{Tag: tag, Name: name, Value: val})
}

// AddString appends a string-valued attribute.
func (g *Group) AddString(tag uint8, name, val string) {
	g.Attrs = append(g.Attrs, Attr{Tag: tag, Name: name, Value: []byte(val)})
}

// AddMulti appends a multi-valued string attribute using the
// additional-value encoding (first value named, rest empty-named).
func (g *Group) AddMulti(tag uint8, name string, vals []string) {
	for i, v := range vals {
		n := name
		if i > 0 {
			n = ""
		}
		g.Attrs = append(g.Attrs, Attr{Tag: tag, Name: n, Value: []byte(v)})
	}
}

// AddInt appends a 4-byte integer attribute.
func (g *Group) AddInt(name string, v int32) {
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, uint32(v))
	g.Attrs = append(g.Attrs, Attr{Tag: TagInteger, Name: name, Value: buf})
}

// AddBool appends a boolean attribute.
func (g *Group) AddBool(name string, v bool) {
	b := byte(0)
	if v {
		b = 1
	}
	g.Attrs = append(g.Attrs, Attr{Tag: TagBoolean, Name: name, Value: []byte{b}})
}

// AddEnum appends a 4-byte enum attribute.
func (g *Group) AddEnum(name string, v uint32) {
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, v)
	g.Attrs = append(g.Attrs, Attr{Tag: TagEnum, Name: name, Value: buf})
}

// AddRange appends a rangeOfInteger attribute (8 bytes: lower, upper).
func (g *Group) AddRange(name string, lower, upper uint32) {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint32(buf[0:4], lower)
	binary.BigEndian.PutUint32(buf[4:8], upper)
	g.Attrs = append(g.Attrs, Attr{Tag: TagRange, Name: name, Value: buf})
}

// Message is an IPP request or response.
type Message struct {
	Version   uint16
	Code      uint16 // operation-id (request) or status-code (response)
	RequestID uint32
	Groups    []Group
}

// EncodeRequest serialises a request message followed by the end tag and,
// if data is non-nil, the document body. The returned reader streams the
// prefix from memory and the document from data, so large documents are
// never fully buffered.
func EncodeRequest(m *Message, data io.Reader) (io.Reader, error) {
	var buf bytes.Buffer
	if err := binary.Write(&buf, binary.BigEndian, m.Version); err != nil {
		return nil, err
	}
	if err := binary.Write(&buf, binary.BigEndian, m.Code); err != nil {
		return nil, err
	}
	if err := binary.Write(&buf, binary.BigEndian, m.RequestID); err != nil {
		return nil, err
	}
	for _, g := range m.Groups {
		buf.WriteByte(g.Tag)
		for _, a := range g.Attrs {
			if !isValueTag(a.Tag) {
				return nil, protoErrf(ErrKindIllegalTag, "encode: illegal value tag 0x%02x", a.Tag)
			}
			buf.WriteByte(a.Tag)
			nameLen := uint16(len(a.Name))
			binary.Write(&buf, binary.BigEndian, nameLen)
			buf.WriteString(a.Name)
			valLen := uint16(len(a.Value))
			binary.Write(&buf, binary.BigEndian, valLen)
			buf.Write(a.Value)
		}
	}
	buf.WriteByte(TagEnd)
	if data != nil {
		return io.MultiReader(bytes.NewReader(buf.Bytes()), data), nil
	}
	return bytes.NewReader(buf.Bytes()), nil
}

// Limits applied during decode to bound resource use.
const (
	MaxAttrs     = 4096
	MaxStrLen    = 8192
	MaxValLen    = 65535
)

// DecodeResponse reads and strictly validates a response message from r.
// It stops at the end-of-attributes tag; any remaining bytes (document
// data) are left unread.
func DecodeResponse(r io.Reader, wantRequestID uint32) (*Message, error) {
	header := make([]byte, 8)
	if _, err := io.ReadFull(r, header); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil, protoErrf(ErrKindTruncated, "short header")
		}
		return nil, protoErrf(ErrKindNetwork, "read header: %v", err)
	}
	m := &Message{
		Version:   binary.BigEndian.Uint16(header[0:2]),
		Code:      binary.BigEndian.Uint16(header[2:4]),
		RequestID: binary.BigEndian.Uint32(header[4:8]),
	}
	if m.RequestID != wantRequestID {
		return nil, protoErrf(ErrKindRequestID, "want %d got %d", wantRequestID, m.RequestID)
	}

	seenGroups := map[uint8]bool{}
	attrCount := 0
	var cur *Group
	prevName := ""

	for {
		b, err := readByte(r)
		if err != nil {
			return nil, protoErrf(ErrKindTruncated, "reading tag: %v", err)
		}
		if b == TagEnd {
			break
		}
		if isGroupTag(b) {
			if seenGroups[b] {
				return nil, protoErrf(ErrKindDuplicateGroup, "duplicate group 0x%02x", b)
			}
			seenGroups[b] = true
			cur = &Group{Tag: b}
			m.Groups = append(m.Groups, Group{})
			// point cur at the freshly appended group slice element
			m.Groups[len(m.Groups)-1] = Group{Tag: b}
			cur = &m.Groups[len(m.Groups)-1]
			prevName = ""
			continue
		}
		// otherwise it must be a value tag introducing an attribute
		if !isValueTag(b) {
			return nil, protoErrf(ErrKindIllegalTag, "unexpected tag 0x%02x (expected group or value tag)", b)
		}
		if cur == nil {
			return nil, protoErrf(ErrKindAttrWithoutGroup, "attribute before any group tag")
		}
		attrCount++
		if attrCount > MaxAttrs {
			return nil, protoErrf(ErrKindOversized, "attribute count exceeds %d", MaxAttrs)
		}
		name, err := readLenString(r, "name")
		if err != nil {
			return nil, err
		}
		val, err := readValue(r)
		if err != nil {
			return nil, err
		}
		// Additional values carry an empty name and attach to prevName.
		effectiveName := name
		if name == "" {
			effectiveName = prevName
		} else {
			// A non-empty name appearing more than once in the same
			// group is a duplicate scalar and is rejected.
			for _, a := range cur.Attrs {
				if a.Name == name {
					return nil, protoErrf(ErrKindDuplicateScalar, "duplicate attribute %q", name)
				}
			}
			prevName = name
		}
		if isStringTag(b) && !utf8.Valid(val) {
			return nil, protoErrf(ErrKindBadUTF8, "attribute %q is not valid utf-8", effectiveName)
		}
		cur.Attrs = append(cur.Attrs, Attr{Tag: b, Name: effectiveName, Value: val})
	}
	return m, nil
}

// isStringTag reports whether the tag's value is textual and must be UTF-8.
func isStringTag(b uint8) bool {
	switch b {
	case TagText, TagName, TagKeyword, TagURI, TagURIScheme, TagCharset, TagNaturalLang, TagMime:
		return true
	}
	return false
}

func readByte(r io.Reader) (uint8, error) {
	var b [1]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}
	return b[0], nil
}

func readUint16(r io.Reader) (uint16, error) {
	var b [2]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint16(b[:]), nil
}

func readLenString(r io.Reader, what string) (string, error) {
	n, err := readUint16(r)
	if err != nil {
		return "", protoErrf(ErrKindTruncated, "reading %s length: %v", what, err)
	}
	if int(n) > MaxStrLen {
		return "", protoErrf(ErrKindOversized, "%s length %d exceeds %d", what, n, MaxStrLen)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", protoErrf(ErrKindTruncated, "reading %s: %v", what, err)
	}
	return string(buf), nil
}

func readValue(r io.Reader) ([]byte, error) {
	n, err := readUint16(r)
	if err != nil {
		return nil, protoErrf(ErrKindTruncated, "reading value length: %v", err)
	}
	if int(n) > MaxValLen {
		return nil, protoErrf(ErrKindOversized, "value length %d exceeds %d", n, MaxValLen)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, protoErrf(ErrKindTruncated, "reading value: %v", err)
	}
	return buf, nil
}

// Find returns the first attribute matching name in the first group of
// the given tag, or nil.
func (m *Message) Find(groupTag uint8, name string) *Attr {
	for i := range m.Groups {
		if m.Groups[i].Tag != groupTag {
			continue
		}
		for j := range m.Groups[i].Attrs {
			if m.Groups[i].Attrs[j].Name == name {
				return &m.Groups[i].Attrs[j]
			}
		}
	}
	return nil
}

// FindAll returns all attribute values for name within the first group
// of the given tag.
func (m *Message) FindAll(groupTag uint8, name string) [][]byte {
	var out [][]byte
	for i := range m.Groups {
		if m.Groups[i].Tag != groupTag {
			continue
		}
		for j := range m.Groups[i].Attrs {
			if m.Groups[i].Attrs[j].Name == name {
				out = append(out, m.Groups[i].Attrs[j].Value)
			}
		}
		break
	}
	return out
}

// AsInt decodes a 4-byte integer attribute value.
func AsInt(v []byte) (int32, bool) {
	if len(v) != 4 {
		return 0, false
	}
	return int32(binary.BigEndian.Uint32(v)), true
}

// AsEnum decodes a 4-byte enum attribute value.
func AsEnum(v []byte) (uint32, bool) {
	if len(v) != 4 {
		return 0, false
	}
	return binary.BigEndian.Uint32(v), true
}

// AsBool decodes a boolean attribute value.
func AsBool(v []byte) (bool, bool) {
	if len(v) != 1 {
		return false, false
	}
	return v[0] != 0, true
}

// AsString decodes a string attribute value.
func AsString(v []byte) (string, bool) {
	return string(v), true
}

// AsRange decodes a rangeOfInteger attribute value (lower, upper).
func AsRange(v []byte) (lower, upper uint32, ok bool) {
	if len(v) != 8 {
		return 0, 0, false
	}
	return binary.BigEndian.Uint32(v[0:4]), binary.BigEndian.Uint32(v[4:8]), true
}
