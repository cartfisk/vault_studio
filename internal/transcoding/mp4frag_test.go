package transcoding

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// box builds a 32-bit-size MP4 box.
func box(typ string, payloadLen int) []byte {
	b := make([]byte, 8+payloadLen)
	binary.BigEndian.PutUint32(b[0:4], uint32(8+payloadLen))
	copy(b[4:8], typ)
	return b
}

// largeBox builds a 64-bit-largesize MP4 box (size field == 1).
func largeBox(typ string, payloadLen int) []byte {
	b := make([]byte, 16+payloadLen)
	binary.BigEndian.PutUint32(b[0:4], 1)
	copy(b[4:8], typ)
	binary.BigEndian.PutUint64(b[8:16], uint64(16+payloadLen))
	return b
}

// ftyp(16) moov(16) | moof(16) mdat(40) | moof(16) mdat(40)
func twoFragmentFile() []byte {
	var buf bytes.Buffer
	buf.Write(box("ftyp", 8))
	buf.Write(box("moov", 8))
	buf.Write(box("moof", 8))
	buf.Write(box("mdat", 32))
	buf.Write(box("moof", 8))
	buf.Write(box("mdat", 32))
	return buf.Bytes()
}

func TestScanFragmentsTwoFragments(t *testing.T) {
	data := twoFragmentFile()
	got, err := ScanFragments(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("ScanFragments() error = %v", err)
	}

	if got.InitByteEnd != 31 {
		t.Errorf("InitByteEnd = %d, want 31", got.InitByteEnd)
	}
	want := []Fragment{{Start: 32, End: 87}, {Start: 88, End: 143}}
	if len(got.Fragments) != len(want) {
		t.Fatalf("Fragments = %d, want %d", len(got.Fragments), len(want))
	}
	for i := range want {
		if got.Fragments[i] != want[i] {
			t.Errorf("Fragments[%d] = %+v, want %+v", i, got.Fragments[i], want[i])
		}
	}
}

func TestScanFragmentsLastFragmentEndsAtEOF(t *testing.T) {
	data := twoFragmentFile()
	got, err := ScanFragments(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("ScanFragments() error = %v", err)
	}
	last := got.Fragments[len(got.Fragments)-1]
	if last.End != int64(len(data))-1 {
		t.Errorf("last fragment End = %d, want %d", last.End, len(data)-1)
	}
}

func TestScanFragmentsLargeSizeBox(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(box("ftyp", 8))
	buf.Write(box("moov", 8))
	buf.Write(box("moof", 8))
	buf.Write(largeBox("mdat", 32)) // 48 bytes
	data := buf.Bytes()

	got, err := ScanFragments(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("ScanFragments() error = %v", err)
	}
	if len(got.Fragments) != 1 {
		t.Fatalf("Fragments = %d, want 1", len(got.Fragments))
	}
	if got.Fragments[0] != (Fragment{Start: 32, End: 95}) {
		t.Errorf("Fragments[0] = %+v, want {32 95}", got.Fragments[0])
	}
}

func TestScanFragmentsTruncatedFile(t *testing.T) {
	data := twoFragmentFile()
	truncated := data[:len(data)-10] // final mdat claims more bytes than exist

	if _, err := ScanFragments(bytes.NewReader(truncated), int64(len(truncated))); err == nil {
		t.Fatal("ScanFragments() error = nil, want error for truncated file")
	}
}

func TestScanFragmentsNoFragments(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(box("ftyp", 8))
	buf.Write(box("moov", 8))
	data := buf.Bytes()

	if _, err := ScanFragments(bytes.NewReader(data), int64(len(data))); err == nil {
		t.Fatal("ScanFragments() error = nil, want error when no moof is present")
	}
}

func TestScanFragmentsUndersizedBox(t *testing.T) {
	b := make([]byte, 8)
	binary.BigEndian.PutUint32(b[0:4], 4) // smaller than the 8-byte header
	copy(b[4:8], "ftyp")

	if _, err := ScanFragments(bytes.NewReader(b), int64(len(b))); err == nil {
		t.Fatal("ScanFragments() error = nil, want error for undersized box")
	}
}
