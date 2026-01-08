package otel

import (
	"bytes"
	"context"
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

	// Verify content using reader
	ctx := context.Background()
	reader, err := NewTraceFileReader(ctx, tmpFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	count := 0
	err = reader.VisitAll(ctx, func(msg proto.Message) error {
		count++
		if _, ok := msg.(*coltracepb.ExportTraceServiceRequest); !ok {
			t.Errorf("Expected *coltracepb.ExportTraceServiceRequest, got %T", msg)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if count != 1 {
		t.Errorf("Expected 1 message, got %d", count)
	}
}
