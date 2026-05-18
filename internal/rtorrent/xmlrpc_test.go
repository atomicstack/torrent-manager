package rtorrent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestXMLRPCEncode_NoArgs(t *testing.T) {
	got := encodeXMLRPCCall("system.client_version")
	assert.Contains(t, got, "<methodName>system.client_version</methodName>")
	assert.Contains(t, got, "<params></params>")
}

func TestXMLRPCEncode_StringAndBase64(t *testing.T) {
	got := encodeXMLRPCCall("load.raw_verbose", xmlString(""), xmlBase64([]byte("torrent-bytes")))
	assert.Contains(t, got, "<methodName>load.raw_verbose</methodName>")
	assert.Contains(t, got, "<value><string></string></value>")
	assert.Contains(t, got, "<value><base64>dG9ycmVudC1ieXRlcw==</base64></value>")
}

func TestXMLRPCDecode_StringResponse(t *testing.T) {
	resp := []byte("Content-Type: text/xml\r\n\r\n<methodResponse><params><param><value><string>0.9.8</string></value></param></params></methodResponse>")
	v, err := decodeXMLRPCResponse(resp)
	require.NoError(t, err)
	assert.Equal(t, "0.9.8", v)
}

func TestXMLRPCDecode_FaultResponse(t *testing.T) {
	resp := []byte("Content-Type: text/xml\r\n\r\n<methodResponse><fault><value><struct><member><name>faultCode</name><value><i4>-501</i4></value></member><member><name>faultString</name><value><string>Could not create download</string></value></member></struct></value></fault></methodResponse>")
	_, err := decodeXMLRPCResponse(resp)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Could not create download")
	assert.Contains(t, err.Error(), "-501")
}
