package wire

import (
	"bytes"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestEncodeDecode_FileManifest(t *testing.T) {
	p := &FileManifestPayload{
		MsgID:   uuid.New().String(),
		GroupID: uuid.New().String(),
		Caption: "тест 123",
		Files: []FileManifestEntry{{
			Sha:        strings.Repeat("a", 64),
			Size:       12345,
			ChunkSize:  512 * 1024,
			ChunkCount: 1,
			Chunks:     []string{strings.Repeat("b", 64)},
			Mime:       "image/jpeg",
			Width:      1920,
			Height:     1080,
			Name:       "photo.jpg",
			Kind:       "photo",
		}},
	}
	frame, err := EncodeFileManifest(p)
	if err != nil {
		t.Fatal(err)
	}
	if FrameType(frame) != TypeFileManifest {
		t.Fatal("bad type")
	}
	got, err := DecodeFileManifest(frame)
	if err != nil {
		t.Fatal(err)
	}
	if got.MsgID != p.MsgID || got.GroupID != p.GroupID || got.Caption != p.Caption {
		t.Fatal("meta mismatch")
	}
	if len(got.Files) != 1 || got.Files[0].Sha != p.Files[0].Sha {
		t.Fatal("files mismatch")
	}
}

func TestEncodeDecode_FileManifestDM_NoGroup(t *testing.T) {
	p := &FileManifestPayload{MsgID: uuid.New().String(), Files: []FileManifestEntry{}}
	frame, _ := EncodeFileManifest(p)
	got, err := DecodeFileManifest(frame)
	if err != nil {
		t.Fatal(err)
	}
	if got.GroupID != "" {
		t.Fatalf("expected empty groupID, got %q", got.GroupID)
	}
}

func TestEncodeDecode_ChunkRequest(t *testing.T) {
	sha := strings.Repeat("c", 64)
	frame, err := EncodeChunkRequest(sha, 42, 8)
	if err != nil {
		t.Fatal(err)
	}
	gotSha, idx, count, err := DecodeChunkRequest(frame)
	if err != nil {
		t.Fatal(err)
	}
	if gotSha != sha || idx != 42 || count != 8 {
		t.Fatalf("got %s %d %d", gotSha, idx, count)
	}
}

func TestEncodeDecode_ChunkData(t *testing.T) {
	sha := strings.Repeat("d", 64)
	payload := bytes.Repeat([]byte{0x42}, 4096)
	frame, err := EncodeChunkData(sha, 100, payload)
	if err != nil {
		t.Fatal(err)
	}
	gotSha, idx, gotPayload, err := DecodeChunkData(frame)
	if err != nil {
		t.Fatal(err)
	}
	if gotSha != sha || idx != 100 {
		t.Fatal("meta mismatch")
	}
	if !bytes.Equal(gotPayload, payload) {
		t.Fatal("payload mismatch")
	}
}

func TestEncodeDecode_ChunkNak(t *testing.T) {
	sha := strings.Repeat("e", 64)
	frame, _ := EncodeChunkNak(sha, 5, NakRemoved)
	gotSha, idx, reason, err := DecodeChunkNak(frame)
	if err != nil {
		t.Fatal(err)
	}
	if gotSha != sha || idx != 5 || reason != NakRemoved {
		t.Fatal("mismatch")
	}
}

func TestEncodeDecode_FileDecline(t *testing.T) {
	id := uuid.New().String()
	frame, _ := EncodeFileDecline(id)
	got, err := DecodeFileDecline(frame)
	if err != nil {
		t.Fatal(err)
	}
	if got != id {
		t.Fatalf("got %s want %s", got, id)
	}
}

func TestFrameType_BadInputs(t *testing.T) {
	if FrameType(nil) != 0 {
		t.Fatal("nil frame")
	}
	if FrameType([]byte{}) != 0 {
		t.Fatal("empty frame")
	}
	if _, _, _, err := DecodeChunkRequest([]byte{0x11}); err == nil {
		t.Fatal("expected error for short frame")
	}
}
