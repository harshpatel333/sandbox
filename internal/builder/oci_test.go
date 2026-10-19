package builder

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestExtractTarStopsAtSizeLimit(t *testing.T) {
	t.Parallel()

	const limit = 1 << 10
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	body := bytes.Repeat([]byte("x"), 1<<20)
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "big", Mode: 0o644, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	dest := t.TempDir()
	if err := extractTar(context.Background(), &buf, dest, limit); err == nil {
		t.Fatal("expected size limit error")
	}
	info, err := os.Stat(filepath.Join(dest, "big"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > limit+1 {
		t.Fatalf("wrote %d bytes, want at most %d", info.Size(), limit+1)
	}
}
