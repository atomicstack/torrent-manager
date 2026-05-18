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
				code = firstNonEmpty(m.Value.I4, m.Value.Int, m.Value.String)
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
	return firstNonEmpty(v.String, v.I4, v.Int), nil
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
