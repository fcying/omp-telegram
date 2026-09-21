package media

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSnapshotConfinesSource(t *testing.T) {
	workspace, outside, spool := t.TempDir(), t.TempDir(), t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(workspace, "pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{secret, "../" + filepath.Base(outside) + "/secret", "escape/secret", "pipe", "."} {
		if _, err := Snapshot(context.Background(), workspace, spool, path, "document", ""); err == nil {
			t.Errorf("accepted unsafe source %q", path)
		}
	}
	entries, err := os.ReadDir(spool)
	if err != nil || len(entries) != 0 {
		t.Fatalf("rejected sources left snapshots: %v, %v", entries, err)
	}
}

func TestSnapshotRetainsImmutableCopy(t *testing.T) {
	workspace, spool := t.TempDir(), t.TempDir()
	path := filepath.Join(workspace, "original.txt")
	if err := os.WriteFile(path, []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("original.txt", filepath.Join(workspace, "inside")); err != nil {
		t.Fatal(err)
	}
	file, err := Snapshot(context.Background(), workspace, spool, "inside", "document", "caption")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("after"), 0600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file.Path)
	if err != nil || string(data) != "before" {
		t.Fatalf("snapshot changed with source: %q, %v", data, err)
	}
	info, err := os.Stat(file.Path)
	if err != nil || info.Mode().Perm()&0222 != 0 {
		t.Fatalf("snapshot is writable: %v, %v", info, err)
	}
}

func TestSnapshotRejectsSourceChangedDuringCopy(t *testing.T) {
	workspace, spool := t.TempDir(), t.TempDir()
	path := filepath.Join(workspace, "large.txt")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(MaxDocumentBytes); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	mutatorErr := make(chan error, 1)
	go func() {
		time.Sleep(time.Millisecond)
		mutatorErr <- os.Truncate(path, MaxDocumentBytes-1)
	}()
	_, snapshotErr := Snapshot(context.Background(), workspace, spool, path, "document", "")
	if err := <-mutatorErr; err != nil {
		t.Fatal(err)
	}
	if snapshotErr == nil || !strings.Contains(snapshotErr.Error(), "changed during snapshot") {
		t.Fatalf("changed source was accepted: %v", snapshotErr)
	}
}

func TestSnapshotBudgetsAndCancellation(t *testing.T) {
	workspace, spool := t.TempDir(), t.TempDir()
	path := filepath.Join(workspace, "file")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(MaxDocumentBytes + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := Snapshot(context.Background(), workspace, spool, path, "document", ""); err == nil {
		t.Fatal("accepted oversized document")
	}
	if err := os.WriteFile(path, []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Snapshot(context.Background(), workspace, spool, path, "document", strings.Repeat("\U0001F600", 513)); err == nil {
		t.Fatal("caption counted runes instead of UTF-16 units")
	}
	file, err := Snapshot(context.Background(), workspace, spool, path, "document", strings.Repeat("\U0001F600", 512))
	if err != nil {
		t.Fatalf("rejected exact caption boundary: %v", err)
	}
	os.Remove(file.Path)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Snapshot(ctx, workspace, spool, path, "document", ""); err != context.Canceled {
		t.Fatalf("cancellation = %v", err)
	}
	entries, err := os.ReadDir(spool)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed snapshots leaked: %v, %v", entries, err)
	}
}

func TestSafeFilenameRemovesUnicodeFormattingControls(t *testing.T) {
	for _, r := range []rune{'\u202e', '\u2028', '\u2029', '\u200d'} {
		name := "report" + string(r) + ".txt"
		if got := SafeFilename(name); strings.ContainsRune(got, r) {
			t.Fatalf("SafeFilename retained formatting rune U+%04X in %q", r, got)
		}
	}
}

func TestImageInlineAndPreview(t *testing.T) {
	for _, large := range []bool{false, true} {
		var encoded bytes.Buffer
		if large {
			img := image.NewNRGBA(image.Rect(0, 0, 1500, 1000))
			rng := rand.New(rand.NewPCG(7, 11))
			for i := 0; i < len(img.Pix); i += 4 {
				img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = byte(rng.IntN(256)), byte(rng.IntN(256)), byte(rng.IntN(256)), 255
			}
			if err := png.Encode(&encoded, img); err != nil {
				t.Fatal(err)
			}
		} else {
			img := image.NewNRGBA(image.Rect(0, 0, 10, 10))
			img.Set(0, 0, color.NRGBA{R: 255, A: 255})
			if err := png.Encode(&encoded, img); err != nil {
				t.Fatal(err)
			}
		}
		original := append([]byte(nil), encoded.Bytes()...)
		path := filepath.Join(t.TempDir(), "misleading.txt")
		if err := os.WriteFile(path, original, 0600); err != nil {
			t.Fatal(err)
		}
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		images, note := imageInput(context.Background(), file)
		file.Close()
		if len(images) != 1 {
			t.Fatalf("no inline image: %s", note)
		}
		data, err := base64.StdEncoding.DecodeString(images[0].Data)
		if err != nil || len(data) > maxInlineBytes {
			t.Fatalf("invalid inline budget: %d, %v", len(data), err)
		}
		if large {
			config, err := jpeg.DecodeConfig(bytes.NewReader(data))
			if err != nil || max(config.Width, config.Height) > 1280 || images[0].MimeType != "image/jpeg" {
				t.Fatalf("invalid preview: %+v, %v", config, err)
			}
		} else if !bytes.Equal(original, data) || images[0].MimeType != "image/png" {
			t.Fatal("small image did not retain original bytes and actual MIME type")
		}
		retained, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(original, retained) {
			t.Fatal("preview modified the original")
		}
	}
}

func pngHeader(width, height uint32) []byte {
	data := make([]byte, 33)
	copy(data, "\x89PNG\r\n\x1a\n")
	binary.BigEndian.PutUint32(data[8:12], 13)
	copy(data[12:16], "IHDR")
	binary.BigEndian.PutUint32(data[16:20], width)
	binary.BigEndian.PutUint32(data[20:24], height)
	data[24], data[25] = 8, 2
	binary.BigEndian.PutUint32(data[29:33], crc32.ChecksumIEEE(data[12:29]))
	return data
}

func TestImageRejectsDecodedPixelBomb(t *testing.T) {
	path := filepath.Join(t.TempDir(), "image.png")
	if err := os.WriteFile(path, pngHeader(4001, 4000), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	images, note := imageInput(context.Background(), file)
	if len(images) != 0 || !strings.Contains(note, "16-million-pixel") {
		t.Fatalf("pixel budget not enforced: %v, %s", images, note)
	}
}

func TestPhotoSnapshotValidation(t *testing.T) {
	workspace, spool := t.TempDir(), t.TempDir()
	path := filepath.Join(workspace, "photo.png")
	for _, data := range [][]byte{[]byte("not an image"), pngHeader(5001, 5000), pngHeader(2000, 10), pngHeader(10, 10)} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Snapshot(context.Background(), workspace, spool, path, "photo", ""); err == nil {
			t.Fatal("accepted invalid or oversized photo")
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 32, 32))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := Snapshot(context.Background(), workspace, spool, path, "photo", "")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file.Path)
	if err != nil || !bytes.Equal(data, encoded.Bytes()) {
		t.Fatal("photo snapshot changed bytes")
	}
}

func TestAttachmentFilenameSafety(t *testing.T) {
	for _, name := range []string{"../../secret", "..\\..\\secret", "..", "/", "a\nfile", strings.Repeat("a", 400)} {
		got := safeName(name)
		if !filepath.IsLocal(got) || filepath.Base(got) != got || len(got) > 160 || strings.ContainsAny(got, "\r\n\\/") {
			t.Fatalf("unsafe filename %q from %q", got, name)
		}
	}
}
