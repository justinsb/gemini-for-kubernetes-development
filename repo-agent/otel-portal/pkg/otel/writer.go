package otel

import (
	"context"
	"encoding/binary"
	"hash/crc32"
	"os"
	"sync"

	"google.golang.org/protobuf/proto"
	"k8s.io/klog/v2"
)

const (
	// Magic number for the file format (4 bytes)
	// Using a placeholder; can be changed if specified.
	MagicNumber = 0x4F54454C // "OTEL" in ASCII
)

type FileWriter struct {
	mu   sync.Mutex
	file *os.File
}

func NewFileWriter(path string) (*FileWriter, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	return &FileWriter{file: f}, nil
}

func (w *FileWriter) Write(ctx context.Context, msg proto.Message) error {
	log := klog.FromContext(ctx)

	data, err := proto.Marshal(msg)
	if err != nil {
		return err
	}

	log.Info("writing data to file")

	w.mu.Lock()
	defer w.mu.Unlock()

	// Header: Magic (4), Length (4), CRC32C (4), Reserved (4)
	// Total 16 bytes.

	length := uint32(len(data))
	crc := crc32.Checksum(data, crc32.MakeTable(crc32.Castagnoli))
	reserved := uint32(0)

	header := make([]byte, 16)
	binary.BigEndian.PutUint32(header[0:4], MagicNumber)
	binary.BigEndian.PutUint32(header[4:8], length)
	binary.BigEndian.PutUint32(header[8:12], crc)
	binary.BigEndian.PutUint32(header[12:16], reserved)

	if _, err := w.file.Write(header); err != nil {
		return err
	}
	if _, err := w.file.Write(data); err != nil {
		return err
	}

	return nil
}

func (w *FileWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.file.Close()
}
