package rtorrent

import (
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"strings"
)

// xmlValue is one positional XML-RPC parameter. Body is interpolated raw into
// the output XML; constructors (xmlString, xmlInt, xmlBase64) are responsible
// for any escaping or encoding needed to make Body XML-safe.
type xmlValue struct {
	Tag  string
	Body string
}

func xmlString(s string) xmlValue { return xmlValue{Tag: "string", Body: xmlEscape(s)} }
func xmlInt(i int) xmlValue       { return xmlValue{Tag: "i4", Body: fmt.Sprintf("%d", i)} }
func xmlBase64(b []byte) xmlValue {
	return xmlValue{Tag: "base64", Body: base64.StdEncoding.EncodeToString(b)}
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func encodeXMLRPCCall(method string, args ...xmlValue) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?>`)
	b.WriteString("<methodCall><methodName>")
	b.WriteString(xmlEscape(method))
	b.WriteString("</methodName><params>")
	for _, a := range args {
		b.WriteString("<param><value><")
		b.WriteString(a.Tag)
		b.WriteString(">")
		b.WriteString(a.Body)
		b.WriteString("</")
		b.WriteString(a.Tag)
		b.WriteString("></value></param>")
	}
	b.WriteString("</params></methodCall>")
	return b.String()
}

// decodeXMLRPCResponse returns the first <param><value> as a string (or the
// representation of whatever scalar type was returned). Returns an error
// wrapping faultCode + faultString on a <fault> response.
func decodeXMLRPCResponse(raw []byte) (string, error) {
	// strip http-style headers ("Content-Type: ...\r\n\r\n") if present.
	// rtorrent's SCGI daemon always uses CRLF line endings; LF-only headers
	// are not handled — a malformed response would surface as a parse error
	// from xml.Unmarshal below, which is acceptable.
	if i := bytes.Index(raw, []byte("\r\n\r\n")); i >= 0 {
		raw = raw[i+4:]
	}

	type valueT struct {
		String string `xml:"string"`
		I4     string `xml:"i4"`
		I8     string `xml:"i8"` // rtorrent 0.9.8 sends 64-bit ints as <i8>
		Int    string `xml:"int"`
	}
	type memberT struct {
		Name  string `xml:"name"`
		Value valueT `xml:"value"`
	}
	type structT struct {
		Members []memberT `xml:"member"`
	}
	type faultT struct {
		Value struct {
			Struct structT `xml:"struct"`
		} `xml:"value"`
	}
	type paramT struct {
		Value valueT `xml:"value"`
	}
	type respT struct {
		XMLName xml.Name `xml:"methodResponse"`
		Params  *struct {
			Param paramT `xml:"param"`
		} `xml:"params"`
		Fault *faultT `xml:"fault"`
	}

	var r respT
	if err := xml.Unmarshal(raw, &r); err != nil {
		return "", fmt.Errorf("parse xmlrpc: %w", err)
	}
	if r.Fault != nil {
		var code, msg string
		for _, m := range r.Fault.Value.Struct.Members {
			switch m.Name {
			case "faultCode":
				code = firstNonEmpty(m.Value.I4, m.Value.I8, m.Value.Int, m.Value.String)
			case "faultString":
				msg = m.Value.String
			}
		}
		return "", fmt.Errorf("rtorrent fault %s: %s", code, msg)
	}
	if r.Params == nil {
		return "", nil
	}
	v := r.Params.Param.Value
	return firstNonEmpty(v.String, v.I4, v.I8, v.Int), nil
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// decodeXMLRPCResponseStringMatrix returns the response as a [][]string for
// d.multicall / d.multicall2 responses where each row is itself an array of
// string values. Used to fetch multiple per-torrent fields in one round-trip.
// Returns an error for fault responses.
func decodeXMLRPCResponseStringMatrix(raw []byte) ([][]string, error) {
	if i := bytes.Index(raw, []byte("\r\n\r\n")); i >= 0 {
		raw = raw[i+4:]
	}

	type cellT struct {
		String string `xml:"string"`
		I4     string `xml:"i4"`
		I8     string `xml:"i8"` // rtorrent 0.9.8 sends 64-bit ints as <i8>
		Int    string `xml:"int"`
	}
	type rowDataT struct {
		Values []cellT `xml:"value"`
	}
	type rowArrayT struct {
		Data rowDataT `xml:"data"`
	}
	type rowValueT struct {
		Array rowArrayT `xml:"array"`
	}
	type outerDataT struct {
		Values []rowValueT `xml:"value"`
	}
	type outerArrayT struct {
		Data outerDataT `xml:"data"`
	}
	type paramT struct {
		Value struct {
			Array outerArrayT `xml:"array"`
		} `xml:"value"`
	}
	type valueT struct {
		String string `xml:"string"`
		I4     string `xml:"i4"`
		I8     string `xml:"i8"` // rtorrent 0.9.8 sends 64-bit ints as <i8>
		Int    string `xml:"int"`
	}
	type memberT struct {
		Name  string `xml:"name"`
		Value valueT `xml:"value"`
	}
	type structT struct {
		Members []memberT `xml:"member"`
	}
	type faultT struct {
		Value struct {
			Struct structT `xml:"struct"`
		} `xml:"value"`
	}
	type respT struct {
		XMLName xml.Name `xml:"methodResponse"`
		Params  *struct {
			Param paramT `xml:"param"`
		} `xml:"params"`
		Fault *faultT `xml:"fault"`
	}

	var r respT
	if err := xml.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("parse xmlrpc: %w", err)
	}
	if r.Fault != nil {
		var code, msg string
		for _, m := range r.Fault.Value.Struct.Members {
			switch m.Name {
			case "faultCode":
				code = firstNonEmpty(m.Value.I4, m.Value.I8, m.Value.Int, m.Value.String)
			case "faultString":
				msg = m.Value.String
			}
		}
		return nil, fmt.Errorf("rtorrent fault %s: %s", code, msg)
	}
	if r.Params == nil {
		return nil, nil
	}
	out := make([][]string, 0, len(r.Params.Param.Value.Array.Data.Values))
	for _, row := range r.Params.Param.Value.Array.Data.Values {
		cells := make([]string, 0, len(row.Array.Data.Values))
		for _, cell := range row.Array.Data.Values {
			cells = append(cells, firstNonEmpty(cell.String, cell.I4, cell.I8, cell.Int))
		}
		out = append(out, cells)
	}
	return out, nil
}

// decodeXMLRPCResponseStringArray returns the response as a []string. Used for
// rtorrent calls like download_list that return an array of info hashes.
// Returns an error for fault responses or for non-array shapes.
func decodeXMLRPCResponseStringArray(raw []byte) ([]string, error) {
	if i := bytes.Index(raw, []byte("\r\n\r\n")); i >= 0 {
		raw = raw[i+4:]
	}

	type valueT struct {
		String string `xml:"string"`
		I4     string `xml:"i4"`
		I8     string `xml:"i8"` // rtorrent 0.9.8 sends 64-bit ints as <i8>
		Int    string `xml:"int"`
	}
	type memberT struct {
		Name  string `xml:"name"`
		Value valueT `xml:"value"`
	}
	type structT struct {
		Members []memberT `xml:"member"`
	}
	type faultT struct {
		Value struct {
			Struct structT `xml:"struct"`
		} `xml:"value"`
	}
	type dataT struct {
		Values []valueT `xml:"value"`
	}
	type arrayT struct {
		Data dataT `xml:"data"`
	}
	type paramT struct {
		Value struct {
			Array arrayT `xml:"array"`
		} `xml:"value"`
	}
	type respT struct {
		XMLName xml.Name `xml:"methodResponse"`
		Params  *struct {
			Param paramT `xml:"param"`
		} `xml:"params"`
		Fault *faultT `xml:"fault"`
	}

	var r respT
	if err := xml.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("parse xmlrpc: %w", err)
	}
	if r.Fault != nil {
		var code, msg string
		for _, m := range r.Fault.Value.Struct.Members {
			switch m.Name {
			case "faultCode":
				code = firstNonEmpty(m.Value.I4, m.Value.I8, m.Value.Int, m.Value.String)
			case "faultString":
				msg = m.Value.String
			}
		}
		return nil, fmt.Errorf("rtorrent fault %s: %s", code, msg)
	}
	if r.Params == nil {
		return nil, nil
	}
	out := make([]string, 0, len(r.Params.Param.Value.Array.Data.Values))
	for _, v := range r.Params.Param.Value.Array.Data.Values {
		s := firstNonEmpty(v.String, v.I4, v.I8, v.Int)
		if s != "" {
			out = append(out, s)
		}
	}
	return out, nil
}
