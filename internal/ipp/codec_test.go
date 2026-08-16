package ipp

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
	"testing"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	m := &Message{Version: Version20, Code: OpCreateJob, RequestID: 7}
	ops := baseRequest("alice")
	ops.AddString(TagURI, "printer-uri", "http://h/ipp")
	ops.AddString(TagName, "job-name", "pressguard:job1:1")
	m.Groups = []Group{ops}
	raw := readAll(t, EncodeRequestMustSucceed(t, m, nil))
	dec, err := DecodeResponse(bytes.NewReader(raw), 7)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dec.Code != OpCreateJob {
		t.Fatalf("code = %x", dec.Code)
	}
	if dec.RequestID != 7 {
		t.Fatalf("rid = %d", dec.RequestID)
	}
	a := dec.Find(TagOperations, "job-name")
	if a == nil || string(a.Value) != "pressguard:job1:1" {
		t.Fatalf("job-name missing: %+v", a)
	}
}

func EncodeRequestMustSucceed(t *testing.T, m *Message, data io.Reader) io.Reader {
	t.Helper()
	r, err := EncodeRequest(m, data)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return r
}

func readAll(t *testing.T, r io.Reader) []byte {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("readall: %v", err)
	}
	return b
}

func TestDecodeRejectsDuplicateScalar(t *testing.T) {
	var buf bytes.Buffer
	binary.Write(&buf, binary.BigEndian, Version20)
	binary.Write(&buf, binary.BigEndian, StatusOK)
	binary.Write(&buf, binary.BigEndian, uint32(1))
	buf.WriteByte(TagJob)
	writeAttr(&buf, TagInteger, "job-id", int32(5))
	writeAttr(&buf, TagInteger, "job-id", int32(6)) // duplicate name
	buf.WriteByte(TagEnd)
	if _, err := DecodeResponse(&buf, 1); err == nil {
		t.Fatal("expected duplicate-scalar error")
	} else if !strings.Contains(err.Error(), "duplicate_scalar") {
		t.Fatalf("wrong error: %v", err)
	}
}

func TestDecodeRejectsIllegalTag(t *testing.T) {
	var buf bytes.Buffer
	binary.Write(&buf, binary.BigEndian, Version20)
	binary.Write(&buf, binary.BigEndian, StatusOK)
	binary.Write(&buf, binary.BigEndian, uint32(1))
	buf.WriteByte(0xFF) // illegal group tag
	buf.WriteByte(TagEnd)
	if _, err := DecodeResponse(&buf, 1); err == nil {
		t.Fatal("expected illegal-tag error")
	}
}

func TestDecodeRejectsTruncated(t *testing.T) {
	var buf bytes.Buffer
	binary.Write(&buf, binary.BigEndian, Version20)
	binary.Write(&buf, binary.BigEndian, StatusOK)
	binary.Write(&buf, binary.BigEndian, uint32(1))
	buf.WriteByte(TagJob)
	writeAttr(&buf, TagInteger, "job-id", int32(5))
	buf.WriteByte(TagName)
	binary.Write(&buf, binary.BigEndian, uint16(7))
	buf.WriteString("job-nam") // short
	if _, err := DecodeResponse(&buf, 1); err == nil {
		t.Fatal("expected truncated error")
	}
}

func TestDecodeRejectsRequestIDMismatch(t *testing.T) {
	var buf bytes.Buffer
	binary.Write(&buf, binary.BigEndian, Version20)
	binary.Write(&buf, binary.BigEndian, StatusOK)
	binary.Write(&buf, binary.BigEndian, uint32(99))
	buf.WriteByte(TagEnd)
	if _, err := DecodeResponse(&buf, 1); err == nil {
		t.Fatal("expected request-id mismatch")
	}
}

func TestDecodeRejectsAttrWithoutGroup(t *testing.T) {
	var buf bytes.Buffer
	binary.Write(&buf, binary.BigEndian, Version20)
	binary.Write(&buf, binary.BigEndian, StatusOK)
	binary.Write(&buf, binary.BigEndian, uint32(1))
	writeAttr(&buf, TagInteger, "job-id", int32(5))
	buf.WriteByte(TagEnd)
	if _, err := DecodeResponse(&buf, 1); err == nil {
		t.Fatal("expected attribute-without-group error")
	}
}

func TestDecodeRejectsDuplicateGroup(t *testing.T) {
	var buf bytes.Buffer
	binary.Write(&buf, binary.BigEndian, Version20)
	binary.Write(&buf, binary.BigEndian, StatusOK)
	binary.Write(&buf, binary.BigEndian, uint32(1))
	buf.WriteByte(TagJob)
	writeAttr(&buf, TagInteger, "job-id", int32(5))
	buf.WriteByte(TagJob) // duplicate job group
	writeAttr(&buf, TagEnum, "job-state", int32(9))
	buf.WriteByte(TagEnd)
	if _, err := DecodeResponse(&buf, 1); err == nil {
		t.Fatal("expected duplicate-group error")
	}
}

func writeAttr(buf *bytes.Buffer, tag uint8, name string, val int32) {
	buf.WriteByte(tag)
	binary.Write(buf, binary.BigEndian, uint16(len(name)))
	buf.WriteString(name)
	var vb [4]byte
	binary.BigEndian.PutUint32(vb[:], uint32(val))
	binary.Write(buf, binary.BigEndian, uint16(4))
	buf.Write(vb[:])
}
