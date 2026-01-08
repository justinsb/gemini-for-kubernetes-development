package otel

import (
	"context"
	"io"
	"net/http"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	// Enable gzip compression for gRPC
	_ "google.golang.org/grpc/encoding/gzip"
)

type Server struct {
	writer *FileWriter
}

func NewServer(writer *FileWriter) *Server {
	return &Server{writer: writer}
}

func (s *Server) RegisterGRPC(srv *grpc.Server) {
	coltracepb.RegisterTraceServiceServer(srv, &traceServer{writer: s.writer})
	colmetricspb.RegisterMetricsServiceServer(srv, &metricsServer{writer: s.writer})
	collogspb.RegisterLogsServiceServer(srv, &logsServer{writer: s.writer})
}

func (s *Server) HTTPHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/traces", s.handleTraces)
	mux.HandleFunc("/v1/metrics", s.handleMetrics)
	mux.HandleFunc("/v1/logs", s.handleLogs)
	return mux
}

// --- Trace Service ---

type traceServer struct {
	coltracepb.UnimplementedTraceServiceServer
	writer *FileWriter
}

func (s *traceServer) Export(ctx context.Context, req *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceResponse, error) {
	if err := s.writer.Write(ctx, req); err != nil {
		return nil, err
	}
	return &coltracepb.ExportTraceServiceResponse{}, nil
}

func (s *Server) handleTraces(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// TODO: Check Content-Type (application/x-protobuf)
	// For now assume protobuf

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var req coltracepb.ExportTraceServiceRequest
	if err := proto.Unmarshal(body, &req); err != nil {
		http.Error(w, "Failed to unmarshal body", http.StatusBadRequest)
		return
	}

	if err := s.writer.Write(ctx, &req); err != nil {
		http.Error(w, "Failed to write data", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/x-protobuf")
	resp := &coltracepb.ExportTraceServiceResponse{}
	data, _ := proto.Marshal(resp)
	w.Write(data)
}

// --- Metrics Service ---

type metricsServer struct {
	colmetricspb.UnimplementedMetricsServiceServer
	writer *FileWriter
}

func (s *metricsServer) Export(ctx context.Context, req *colmetricspb.ExportMetricsServiceRequest) (*colmetricspb.ExportMetricsServiceResponse, error) {
	if err := s.writer.Write(ctx, req); err != nil {
		return nil, err
	}
	return &colmetricspb.ExportMetricsServiceResponse{}, nil
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var req colmetricspb.ExportMetricsServiceRequest
	if err := proto.Unmarshal(body, &req); err != nil {
		http.Error(w, "Failed to unmarshal body", http.StatusBadRequest)
		return
	}

	if err := s.writer.Write(ctx, &req); err != nil {
		http.Error(w, "Failed to write data", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/x-protobuf")
	resp := &colmetricspb.ExportMetricsServiceResponse{}
	data, _ := proto.Marshal(resp)
	w.Write(data)
}

// --- Logs Service ---

type logsServer struct {
	collogspb.UnimplementedLogsServiceServer
	writer *FileWriter
}

func (s *logsServer) Export(ctx context.Context, req *collogspb.ExportLogsServiceRequest) (*collogspb.ExportLogsServiceResponse, error) {
	if err := s.writer.Write(ctx, req); err != nil {
		return nil, err
	}
	return &collogspb.ExportLogsServiceResponse{}, nil
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var req collogspb.ExportLogsServiceRequest
	if err := proto.Unmarshal(body, &req); err != nil {
		http.Error(w, "Failed to unmarshal body", http.StatusBadRequest)
		return
	}

	if err := s.writer.Write(ctx, &req); err != nil {
		http.Error(w, "Failed to write data", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/x-protobuf")
	resp := &collogspb.ExportLogsServiceResponse{}
	data, _ := proto.Marshal(resp)
	w.Write(data)
}
