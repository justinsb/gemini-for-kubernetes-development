package otel

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestServer_HTTP_Traces(t *testing.T) {
	ctx := t.Context()

	tmpDir, err := os.MkdirTemp("", "otel-test-dir-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	writer, err := NewFileWriter(ctx, tmpDir, 0)
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
	reader, err := NewTraceFileReader(ctx, tmpDir)
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
