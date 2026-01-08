package otel

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestServer_HTTP_Traces(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "otel-test-*.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())
	tmpFile.Close() // Close so writer can open it

	writer, err := NewFileWriter(tmpFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()

	srv := NewServer(writer)
	handler := srv.HTTPHandler()

	// Create a request
	reqProto := &coltracepb.ExportTraceServiceRequest{}
	reqBytes, _ := proto.Marshal(reqProto)

	req := httptest.NewRequest(http.MethodPost, "/v1/traces", bytes.NewReader(reqBytes))
	req.Header.Set("Content-Type", "application/x-protobuf")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("Expected 200, got %d", w.Code)
	}

	// Check file content
	content, err := os.ReadFile(tmpFile.Name())
	if err != nil {
		t.Fatal(err)
	}

	if len(content) < 16 {
		t.Fatalf("File too short: %d", len(content))
	}

	// Verify Header
	magic := binary.BigEndian.Uint32(content[0:4])
	length := binary.BigEndian.Uint32(content[4:8])
	crc := binary.BigEndian.Uint32(content[8:12])
	reserved := binary.BigEndian.Uint32(content[12:16])

	if magic != MagicNumber {
		t.Errorf("Expected magic %x, got %x", MagicNumber, magic)
	}
	if length != uint32(len(reqBytes)) {
		t.Errorf("Expected length %d, got %d", len(reqBytes), length)
	}

	// Verify Payload
	payload := content[16:]
	if len(payload) != int(length) {
		t.Errorf("Expected payload length %d, got %d", length, len(payload))
	}

	expectedCRC := crc32.Checksum(payload, crc32.MakeTable(crc32.Castagnoli))
	if crc != expectedCRC {
		t.Errorf("Expected CRC %x, got %x", expectedCRC, crc)
	}

	if reserved != 0 {
		t.Errorf("Expected reserved 0, got %d", reserved)
	}
}
