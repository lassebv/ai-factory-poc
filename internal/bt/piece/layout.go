package piece

// Layout geometry.

// PieceSize returns the size of piece i (see Layout); 0 for i out of range.
func (l Layout) PieceSize(i int) int64 {
	if i < 0 || i >= l.NumPieces {
		return 0
	}
	if i < l.NumPieces-1 {
		return l.PieceLength
	}
	s := l.TotalLength - int64(l.NumPieces-1)*l.PieceLength
	if s < 0 {
		return 0
	}
	return s
}

// NumBlocks returns ceil(PieceSize(i)/BlockSize); 0 for i out of range.
func (l Layout) NumBlocks(i int) int {
	return int((l.PieceSize(i) + BlockSize - 1) / BlockSize)
}

// blockLen returns the length of block j of piece i.
func (l Layout) blockLen(i, j int) uint32 {
	rest := l.PieceSize(i) - int64(j)*BlockSize
	if rest > BlockSize {
		return BlockSize
	}
	return uint32(rest)
}

func pipelineDepth(rate int64) int {
	if rate <= 0 {
		return MinPipelineDepth
	}
	d := rate / BlockSize * 2
	if r := rate % BlockSize; r != 0 {
		// rate*2/BlockSize without overflowing for huge rates.
		d += r * 2 / BlockSize
	}
	if d < MinPipelineDepth {
		return MinPipelineDepth
	}
	if d > MaxPipelineDepth {
		return MaxPipelineDepth
	}
	return int(d)
}
