package distribution

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"errors"
	"math"
)

// The release format uses single-disk ZIP32. Inspect the physical directory
// before archive/zip allocates one object for each untrusted entry.
func (limits Limits) openArchive(data []byte) (*zip.Reader, error) {
	const endSignature = 0x06054b50
	var end struct {
		Signature                                 uint32
		Disk, DirectoryDisk, DiskEntries, Entries uint16
		Size, Offset                              uint32
		CommentBytes                              uint16
	}
	endSize := binary.Size(end)
	if len(data) < endSize {
		return nil, ErrInvalid
	}
	endOffset := len(data) - endSize
	if _, err := binary.Decode(data[endOffset:], binary.LittleEndian, &end); err != nil {
		return nil, errors.Join(ErrInvalid, err)
	}
	if end.Signature != endSignature || end.CommentBytes != 0 || end.Disk != 0 || end.DirectoryDisk != 0 || end.Entries != end.DiskEntries || end.Entries == math.MaxUint16 || end.Offset == math.MaxUint32 || end.Size == math.MaxUint32 || uint64(end.Offset)+uint64(end.Size) != uint64(endOffset) {
		return nil, ErrInvalid
	}
	if int(end.Entries) > limits.Entries {
		return nil, ErrCapacity
	}
	if err := limits.checkDirectory(data[end.Offset:endOffset], int(end.Entries)); err != nil {
		return nil, err
	}
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, errors.Join(ErrInvalid, err)
	}
	return reader, nil
}

func (limits Limits) checkDirectory(directory []byte, expected int) error {
	const headerSignature = 0x02014b50
	var header struct {
		Signature                           uint32
		_                                   [24]byte // ZIP central header fields through uncompressed size.
		NameBytes, ExtraBytes, CommentBytes uint16
		_                                   [12]byte // Disk, attributes and local header offset.
	}
	count := 0
	for len(directory) != 0 {
		if count == limits.Entries {
			return ErrCapacity
		}
		fixed, err := binary.Decode(directory, binary.LittleEndian, &header)
		if err != nil || header.Signature != headerSignature {
			return ErrInvalid
		}
		width := fixed + int(header.NameBytes) + int(header.ExtraBytes) + int(header.CommentBytes)
		if width > len(directory) {
			return ErrInvalid
		}
		directory = directory[width:]
		count++
	}
	if count != expected {
		return ErrInvalid
	}
	return nil
}

func (limits Limits) admitsExpansion(expanded, compressed uint64) bool {
	if expanded > uint64(limits.ExpandedBytes) {
		return false
	}
	if compressed == 0 {
		return expanded == 0
	}
	whole, remainder := expanded/compressed, expanded%compressed
	return whole < limits.CompressionRatio || whole == limits.CompressionRatio && remainder == 0
}
