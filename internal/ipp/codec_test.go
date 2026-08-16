package ipp

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net/http"
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

// captureRT records the request body sent by the client and replies with a
// minimal IPP success response whose request-id matches the request.
type captureRT struct{ body []byte }

func (rt *captureRT) RoundTrip(req *http.Request) (*http.Response, error) {
	b, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	rt.body = b
	var resp bytes.Buffer
	binary.Write(&resp, binary.BigEndian, Version20)
	binary.Write(&resp, binary.BigEndian, StatusOK)
	binary.Write(&resp, binary.BigEndian, binary.BigEndian.Uint32(b[4:8])) // echo request-id
	resp.WriteByte(TagEnd)
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/ipp"}},
		Body:       io.NopCloser(bytes.NewReader(resp.Bytes())),
	}, nil
}

// pageRangeNameLens scans the raw IPP request body and returns the name
// lengths of every rangeOfInteger attribute in the job-attributes group,
// in wire order. This exposes the additional-value encoding (first value
// named, subsequent values empty-named) that DecodeResponse normalises
// away and therefore cannot be checked through a round trip.
func pageRangeNameLens(t *testing.T, body []byte) []int {
	t.Helper()
	r := bytes.NewReader(body)
	if _, err := io.ReadFull(r, make([]byte, 8)); err != nil { // skip header
		t.Fatalf("skip header: %v", err)
	}
	inJob := false
	var lens []int
	for {
		b, err := r.ReadByte()
		if err != nil {
			t.Fatalf("read tag: %v", err)
		}
		if b == TagEnd {
			break
		}
		if isGroupTag(b) {
			inJob = b == TagJob
			continue
		}
		if !isValueTag(b) {
			t.Fatalf("unexpected value tag 0x%02x", b)
		}
		var nameLen uint16
		if err := binary.Read(r, binary.BigEndian, &nameLen); err != nil {
			t.Fatalf("read name length: %v", err)
		}
		if _, err := io.ReadFull(r, make([]byte, nameLen)); err != nil {
			t.Fatalf("read name: %v", err)
		}
		var valLen uint16
		if err := binary.Read(r, binary.BigEndian, &valLen); err != nil {
			t.Fatalf("read value length: %v", err)
		}
		if _, err := io.ReadFull(r, make([]byte, valLen)); err != nil {
			t.Fatalf("read value: %v", err)
		}
		if inJob && b == TagRange {
			lens = append(lens, int(nameLen))
		}
	}
	return lens
}

// TestSendDocumentPageRangesEncoding covers the Send-Document page-ranges
// request construction: a single page, a duplex two-page sheet, and
// several valid ranges. It asserts both that the request round-trips
// through the strict decoder (which rejects duplicate scalar names, the
// exact failure a strict IPP printer raises as client-error-bad-request)
// and that subsequent values use RFC 8010 additional-value encoding
// (empty name) rather than repeating the attribute name.
func TestSendDocumentPageRangesEncoding(t *testing.T) {
	cases := []struct {
		name   string
		ranges string
		want   [][2]uint32
	}{
		{"single page", "3", [][2]uint32{{3, 3}}},
		{"duplex two pages", "1,2", [][2]uint32{{1, 1}, {2, 2}}},
		{"multiple valid ranges", "1-3,5,7-8", [][2]uint32{{1, 3}, {5, 5}, {7, 8}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := &captureRT{}
			c := NewClient("p", "http://printer/ipp", WithHTTPClient(&http.Client{Transport: rt}))
			if err := c.SendDocument(context.Background(), 42, strings.NewReader("PDF"), tc.ranges); err != nil {
				t.Fatalf("SendDocument: %v", err)
			}
			rid := binary.BigEndian.Uint32(rt.body[4:8])
			msg, err := DecodeResponse(bytes.NewReader(rt.body), rid)
			if err != nil {
				t.Fatalf("decode captured request: %v", err)
			}
			vals := msg.FindAll(TagJob, "page-ranges")
			if len(vals) != len(tc.want) {
				t.Fatalf("page-ranges values: got %d, want %d", len(vals), len(tc.want))
			}
			for i, w := range tc.want {
				lo, hi, ok := AsRange(vals[i])
				if !ok || lo != w[0] || hi != w[1] {
					t.Fatalf("range %d: got %d-%d, want %d-%d", i, lo, hi, w[0], w[1])
				}
			}
			lens := pageRangeNameLens(t, rt.body)
			if len(lens) != len(tc.want) {
				t.Fatalf("range name lengths: got %d, want %d", len(lens), len(tc.want))
			}
			for i, nl := range lens {
				switch i {
				case 0:
					if nl != len("page-ranges") {
						t.Fatalf("first page-ranges name length = %d, want %d (first value must carry the name)", nl, len("page-ranges"))
					}
				default:
					if nl != 0 {
						t.Fatalf("page-ranges value %d name length = %d, want 0 (additional-value encoding)", i, nl)
					}
				}
			}
		})
	}
}
