package commands

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/gke-labs/gemini-for-kubernetes-development/repo-agent/otel-portal/pkg/otel"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
)

type OtelListenerOptions struct {
	GRPCPort       int
	HTTPPort       int
	OutputDir      string
	RotateInterval time.Duration
}

func (o *OtelListenerOptions) InitDefaults() {
	if o.GRPCPort == 0 {
		o.GRPCPort = 4317
	}
	if o.HTTPPort == 0 {
		o.HTTPPort = 4318
	}
	if o.OutputDir == "" {
		o.OutputDir = "otel-data"
	}
	if o.RotateInterval == 0 {
		o.RotateInterval = 5 * time.Minute
	}
}

func RunOtelListener(ctx context.Context, opt OtelListenerOptions) error {
	writer, err := otel.NewFileWriter(ctx, opt.OutputDir, opt.RotateInterval)
	if err != nil {
		return fmt.Errorf("failed to create file writer: %w", err)
	}
	defer writer.Close()

	srv := otel.NewServer(writer)

	g, ctx := errgroup.WithContext(ctx)

	// Start gRPC Server
	g.Go(func() error {
		addr := fmt.Sprintf(":%d", opt.GRPCPort)
		lis, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("failed to listen on gRPC port %d: %w", opt.GRPCPort, err)
		}
		grpcServer := grpc.NewServer()
		srv.RegisterGRPC(grpcServer)

		log.Printf("Starting gRPC server on %s", addr)

		// Handle graceful shutdown if context is cancelled
		go func() {
			<-ctx.Done()
			grpcServer.GracefulStop()
		}()

		if err := grpcServer.Serve(lis); err != nil {
			return fmt.Errorf("gRPC server failed: %w", err)
		}
		return nil
	})

	// Start HTTP Server
	g.Go(func() error {
		addr := fmt.Sprintf(":%d", opt.HTTPPort)
		httpServer := &http.Server{
			Addr:    addr,
			Handler: srv.HTTPHandler(),
		}

		log.Printf("Starting HTTP server on %s", addr)

		go func() {
			<-ctx.Done()
			httpServer.Close()
		}()

		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			return fmt.Errorf("HTTP server failed: %w", err)
		}
		return nil
	})

	return g.Wait()
}

func BuildOtelListenerCommand() *cobra.Command {
	opt := OtelListenerOptions{}
	cmd := &cobra.Command{
		Use:   "listen",
		Short: "Listen for OpenTelemetry data (HTTP and gRPC)",
		RunE: func(cmd *cobra.Command, args []string) error {
			// InitDefaults should have been called before binding flags if we wanted default values in help,
			// but here we can just ensure defaults are set if 0/empty.
			opt.InitDefaults()
			return RunOtelListener(cmd.Context(), opt)
		},
	}

	cmd.Flags().IntVar(&opt.GRPCPort, "grpc-port", 4317, "Port for gRPC listener")
	cmd.Flags().IntVar(&opt.HTTPPort, "http-port", 4318, "Port for HTTP listener")
	cmd.Flags().StringVar(&opt.OutputDir, "output-dir", "otel-data", "Path to output directory")
	cmd.Flags().DurationVar(&opt.RotateInterval, "rotate-interval", 5*time.Minute, "Interval to rotate log files")

	return cmd
}
