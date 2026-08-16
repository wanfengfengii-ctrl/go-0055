package ipp

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"testing"

	"pressguard/internal/domain"
)

// extractRequestID reads the 4-byte request-id from an IPP request body
// (bytes 4..8 of the message header) so a canned response can echo it back
// and pass the request-id check in DecodeResponse.
func extractRequestID(body []byte) uint32 {
	if len(body) < 8 {
		return 0
	}
	return binary.BigEndian.Uint32(body[4:8])
}

// encodeResponse serialises an IPP response message (header + groups + end
// tag). IPP requests and responses share the same binary layout, so
// EncodeRequest is reused with a status code as the message Code.
func encodeResponse(m *Message) []byte {
	r, err := EncodeRequest(m, nil)
	if err != nil {
		panic(err)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		panic(err)
	}
	return b
}

// addFinishings appends a multi-valued finishings-supported enum attribute
// to g using the IPP additional-value encoding (first value named, the rest
// empty-named) so the decoder treats it as one multi-valued attribute.
func addFinishings(g *Group, finishings []uint32) {
	for i, f := range finishings {
		name := "finishings-supported"
		if i > 0 {
			name = ""
		}
		buf := make([]byte, 4)
		binary.BigEndian.PutUint32(buf, f)
		g.Attrs = append(g.Attrs, Attr{Tag: TagEnum, Name: name, Value: buf})
	}
}

// printerAttrsResponse builds a Get-Printer-Attributes response whose
// printer-attributes group carries only the given finishings-supported
// values.
func printerAttrsResponse(rid uint32, finishings []uint32) []byte {
	g := Group{Tag: TagPrinter}
	addFinishings(&g, finishings)
	m := &Message{Version: Version20, Code: StatusOK, RequestID: rid, Groups: []Group{g}}
	return encodeResponse(m)
}

// fullPrinterAttrsResponse builds a Get-Printer-Attributes response with a
// complete capability set (media, color, format, resolution) plus the given
// finishings-supported values, so a job's full Supports check can be
// exercised without being blocked on unrelated missing capabilities.
func fullPrinterAttrsResponse(rid uint32, finishings []uint32) []byte {
	g := Group{Tag: TagPrinter}
	g.AddString(TagKeyword, "media-supported", "A4")
	g.AddBool("color-supported", true)
	g.AddString(TagMime, "document-format-supported", "application/pdf")
	res := make([]byte, 9)
	binary.BigEndian.PutUint32(res[0:4], 600)
	binary.BigEndian.PutUint32(res[4:8], 600)
	res[8] = 3 // dots per inch
	g.Add(TagResolution, "printer-resolution-supported", res)
	addFinishings(&g, finishings)
	m := &Message{Version: Version20, Code: StatusOK, RequestID: rid, Groups: []Group{g}}
	return encodeResponse(m)
}

// staticTransport serves a single canned IPP response, echoing back the
// request-id parsed from the incoming request so the decoder accepts it.
type staticTransport struct {
	build func(rid uint32) []byte
}

func (t *staticTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	req.Body.Close()
	if err != nil {
		return nil, err
	}
	resp := t.build(extractRequestID(body))
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/ipp"}},
		Body:       io.NopCloser(bytes.NewReader(resp)),
	}, nil
}

func newClient(build func(rid uint32) []byte) *Client {
	return NewClient("p1", "http://printer/ipp",
		WithHTTPClient(&http.Client{Transport: &staticTransport{build: build}}))
}

// TestFinishingNameMapping pins the standard IPP finishings enum to its
// domain binding name and guards against the shifted-mapping regression.
// Standard IPP finishings enum (RFC 8011): 3=none, 4=staple, 5=punch,
// 7=bind, 8=saddle-stitch. "none" is not a binding capability; values the
// domain does not model (cover=6, edge-stitch=9, fold=10,
// staple-top-left=20) and unknown enums are ignored.
func TestFinishingNameMapping(t *testing.T) {
	cases := []struct {
		enum uint32
		name string
		ok   bool
	}{
		{FinishingsNone, "", false},
		{FinishingsStaple, "staple", true},
		{FinishingsPunch, "punch", true},
		{FinishingsBind, "bind", true},
		{FinishingsSaddleStitch, "saddle-stitch", true},
		{6, "", false},
		{9, "", false},
		{10, "", false},
		{20, "", false},
		{99, "", false},
		{0, "", false},
	}
	for _, tc := range cases {
		buf := make([]byte, 4)
		binary.BigEndian.PutUint32(buf, tc.enum)
		name, ok := finishingName(buf)
		if ok != tc.ok {
			t.Errorf("enum %d: ok=%v want %v", tc.enum, ok, tc.ok)
		}
		if name != tc.name {
			t.Errorf("enum %d: name=%q want %q", tc.enum, name, tc.name)
		}
	}
}

// TestFinishingNameShortValue guards against a truncated enum value being
// misread as a finishing capability.
func TestFinishingNameShortValue(t *testing.T) {
	if name, ok := finishingName([]byte{0x00, 0x00, 0x00}); ok || name != "" {
		t.Fatalf("short value: name=%q ok=%v, want ignored", name, ok)
	}
}

// TestGetPrinterAttributesFinishings exercises the full client parse path:
// a Get-Printer-Attributes response is mapped to domain.Binding, covering a
// printer that supports finishing, one with no finishing capability (only
// "none"), one that omits the attribute, and one reporting only unrecognised
// enum values.
func TestGetPrinterAttributesFinishings(t *testing.T) {
	cases := []struct {
		name       string
		finishings []uint32
		want       []string
	}{
		{
			name:       "supported finishing",
			finishings: []uint32{FinishingsNone, FinishingsStaple, FinishingsPunch, FinishingsBind, FinishingsSaddleStitch},
			want:       []string{"staple", "punch", "bind", "saddle-stitch"},
		},
		{
			name:       "staple only",
			finishings: []uint32{FinishingsNone, FinishingsStaple},
			want:       []string{"staple"},
		},
		{
			name:       "no finishing capability (none only)",
			finishings: []uint32{FinishingsNone},
			want:       nil,
		},
		{
			name:       "attribute absent",
			finishings: nil,
			want:       nil,
		},
		{
			name:       "unknown enums ignored",
			finishings: []uint32{6, 9, 10, 20, 99},
			want:       nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newClient(func(rid uint32) []byte {
				return printerAttrsResponse(rid, tc.finishings)
			})
			pa, err := c.GetPrinterAttributes(context.Background())
			if err != nil {
				t.Fatalf("GetPrinterAttributes: %v", err)
			}
			if !bindingEqual(pa.Cap.Binding, tc.want) {
				t.Fatalf("Binding=%v want %v", pa.Cap.Binding, tc.want)
			}
			if !pa.Online {
				t.Fatalf("Online=false, want true")
			}
		})
	}
}

// TestGetPrinterAttributesFinishingsCapabilityMatch verifies that the parsed
// binding capability drives the domain Supports decision: a staple job is
// satisfied by a staple-capable printer, while a punch requirement is
// rejected because the printer advertises only staple finishing.
func TestGetPrinterAttributesFinishingsCapabilityMatch(t *testing.T) {
	staplePrinter := newClient(func(rid uint32) []byte {
		return fullPrinterAttrsResponse(rid, []uint32{FinishingsNone, FinishingsStaple})
	})
	pa, err := staplePrinter.GetPrinterAttributes(context.Background())
	if err != nil {
		t.Fatalf("GetPrinterAttributes: %v", err)
	}
	spec := domain.JobSpec{
		Media:      "A4",
		Color:      "monochrome",
		Resolution: "600dpi",
		Format:     "application/pdf",
		Binding:    "staple",
	}
	if ok, missing := pa.Cap.Supports(spec); !ok {
		t.Fatalf("staple job on staple printer: missing=%v", missing)
	}
	punchSpec := spec
	punchSpec.Binding = "punch"
	if ok, _ := pa.Cap.Supports(punchSpec); ok {
		t.Fatal("punch job on staple printer should be rejected")
	}
}

func bindingEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
