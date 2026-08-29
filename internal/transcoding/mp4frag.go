package transcoding

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Fragment is one moof/mdat pair, as an inclusive byte range.
// Inclusive because it maps directly onto HTTP "Range: bytes=start-end".
type Fragment struct {
	Start int64
	End   int64
}

// FragmentLayout is the measured structure of a fragmented MP4.
// InitByteEnd is the inclusive end of the file content before the first moof,
// which a client appends once before any fragment.
type FragmentLayout struct {
	InitByteEnd int64
	Fragments   []Fragment
}

// ScanFragments walks the top-level boxes of a fragmented MP4 and records
// where each moof begins. It reports the layout the file actually has, never
// the layout the encoder was asked for.
func ScanFragments(r io.ReaderAt, size int64) (FragmentLayout, error) {
	var layout FragmentLayout
	var moofStarts []int64

	header := make([]byte, 16)
	offset := int64(0)

	for offset < size {
		if size-offset < 8 {
			return layout, fmt.Errorf("truncated box header at offset %d", offset)
		}
		if _, err := r.ReadAt(header[:8], offset); err != nil {
			return layout, fmt.Errorf("read box header at %d: %w", offset, err)
		}

		boxSize := int64(binary.BigEndian.Uint32(header[0:4]))
		boxType := string(header[4:8])

		switch {
		case boxSize == 1:
			if size-offset < 16 {
				return layout, fmt.Errorf("truncated largesize header at offset %d", offset)
			}
			if _, err := r.ReadAt(header[8:16], offset+8); err != nil {
				return layout, fmt.Errorf("read largesize at %d: %w", offset, err)
			}
			boxSize = int64(binary.BigEndian.Uint64(header[8:16]))
			if boxSize < 16 {
				return layout, fmt.Errorf("largesize box at %d claims %d bytes", offset, boxSize)
			}
		case boxSize == 0:
			// Extends to end of file.
			boxSize = size - offset
		case boxSize < 8:
			return layout, fmt.Errorf("box at %d claims %d bytes, minimum is 8", offset, boxSize)
		}

		if boxSize > size-offset {
			return layout, fmt.Errorf(
				"box %q at %d claims %d bytes, past end of file at %d",
				boxType, offset, boxSize, size,
			)
		}

		if boxType == "moof" {
			moofStarts = append(moofStarts, offset)
		}

		offset += boxSize
	}

	if len(moofStarts) == 0 {
		return layout, fmt.Errorf("no moof box found; file is not fragmented")
	}
	if moofStarts[0] == 0 {
		return layout, fmt.Errorf("moof at offset 0; file has no init segment")
	}

	layout.InitByteEnd = moofStarts[0] - 1
	layout.Fragments = make([]Fragment, len(moofStarts))
	for i, start := range moofStarts {
		end := size - 1
		if i+1 < len(moofStarts) {
			end = moofStarts[i+1] - 1
		}
		layout.Fragments[i] = Fragment{Start: start, End: end}
	}

	return layout, nil
}
