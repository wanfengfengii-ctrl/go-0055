package ipp

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"pressguard/internal/domain"
)

func TestGetPrinterAttributesMapsFinishingsSupported(t *testing.T) {
	var printer Group
	printer.Tag = TagPrinter
	printer.AddString(TagKeyword, "media-supported", "A4")
	printer.AddBool("color-supported", true)
	printer.AddString(TagKeyword, "sides-supported", "two-sided-long-edge")
	printer.AddString(TagMime, "document-format-supported", "application/pdf")
	resolution := make([]byte, 9)
	binary.BigEndian.PutUint32(resolution[0:4], 600)
	binary.BigEndian.PutUint32(resolution[4:8], 600)
	resolution[8] = 3 // dots per inch
	printer.Add(TagResolution, "printer-resolution-supported", resolution)
	for i, finishing := range []uint32{3, 4, 5, 7, 8, 999} {
		name := "finishings-supported"
		if i > 0 {
			name = ""
		}
		value := make([]byte, 4)
		binary.BigEndian.PutUint32(value, finishing)
		printer.Add(TagEnum, name, value)
	}

	response := &Message{
		Version:   Version20,
		Code:      StatusOK,
		RequestID: 1,
		Groups:    []Group{printer},
	}
	body := readAll(t, EncodeRequestMustSucceed(t, response, nil))
	httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/ipp"}},
			Body:       io.NopCloser(bytes.NewReader(body)),
		}, nil
	})}
	client := NewClient("printer-1", "http://printer.test/ipp", WithHTTPClient(httpClient))

	attrs, err := client.GetPrinterAttributes(context.Background())
	if err != nil {
		t.Fatalf("GetPrinterAttributes: %v", err)
	}
	wantBindings := []string{"staple", "punch", "bind", "saddle-stitch"}
	if !reflect.DeepEqual(attrs.Cap.Binding, wantBindings) {
		t.Fatalf("Binding = %v, want %v", attrs.Cap.Binding, wantBindings)
	}
	for _, binding := range append(wantBindings, "") {
		spec := domain.JobSpec{
			Media:      "A4",
			Color:      "color",
			Resolution: "600dpi",
			Duplex:     true,
			Format:     "application/pdf",
			Binding:    binding,
		}
		if ok, missing := attrs.Cap.Supports(spec); !ok {
			t.Errorf("Supports(binding=%q) = false, missing %v", binding, missing)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

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
