package logs

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func BenchmarkLogCollectorBacklog(b *testing.B) {
	path := filepath.Join(b.TempDir(), "app.log")
	batch := []byte(strings.Repeat("application handled request successfully\n", 400))
	f, err := os.Create(path)
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()
	c := NewGenericLogCollector(200, 400)
	defer c.Prune(func(string) bool { return false })
	b.ReportAllocs()
	var entries int
	for b.Loop() {
		if err := f.Truncate(0); err != nil {
			b.Fatal(err)
		}
		if _, err := f.WriteAt(batch, 0); err != nil {
			b.Fatal(err)
		}
		if rs := c.readers["app"]; rs != nil {
			rs.lastPos = 0
		}
		for range 2 {
			out, err := c.Collect("app", path, identityMapper)
			if err != nil {
				b.Fatal(err)
			}
			entries += len(out)
		}
	}
	b.ReportMetric(float64(entries)/float64(b.N), "entries/op")
}

func BenchmarkAppLogWriterBatch(b *testing.B) {
	w := NewAppLogWriter(io.Discard, StreamStdout)
	batch := []byte(strings.Repeat("application handled request successfully\n", 200))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := w.Write(batch); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLogRecordEncode(b *testing.B) {
	r := NewRecord(SourceApp, LevelInfo, "application handled request successfully")
	r.App = "api"
	r.AppID = "app_048b3e43-0b6b-4e13-8e68-1f34e746a8c7"
	b.ReportAllocs()
	for b.Loop() {
		if _, err := r.Encode(); err != nil {
			b.Fatal(err)
		}
	}
}
