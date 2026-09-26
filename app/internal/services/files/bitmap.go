package files

// Bitmap — компактное хранилище «получен ли chunk N» для текущего download'а.
// Один bit на chunk. На 40 GB файл с 512 KiB chunks: 80k bits = 10 KB → влезает
// в одну BLOB-row file_downloads.received_bitmap.
//
// Сериализация: байты, bit 0 в байте 0 — chunk #0 получен.
type Bitmap struct {
	bits []byte
	n    int // total chunks
}

// NewBitmap — пустой bitmap на n chunks (все = 0).
func NewBitmap(n int) *Bitmap {
	if n < 0 {
		n = 0
	}
	bytes := (n + 7) / 8
	return &Bitmap{bits: make([]byte, bytes), n: n}
}

// LoadBitmap — восстановить из БД-байт. Если len(data)*8 < n — bitmap расширяется
// нулями справа (защита от truncation).
func LoadBitmap(data []byte, n int) *Bitmap {
	wantBytes := (n + 7) / 8
	bits := make([]byte, wantBytes)
	copy(bits, data)
	return &Bitmap{bits: bits, n: n}
}

// Bytes — для записи в БД.
func (b *Bitmap) Bytes() []byte { return b.bits }

// Total — количество chunks (длина bitmap).
func (b *Bitmap) Total() int { return b.n }

// Set отмечает chunk idx как полученный. Idx out-of-range → no-op.
func (b *Bitmap) Set(idx int) {
	if idx < 0 || idx >= b.n {
		return
	}
	b.bits[idx/8] |= 1 << uint(idx%8)
}

// Get true если chunk idx уже получен.
func (b *Bitmap) Get(idx int) bool {
	if idx < 0 || idx >= b.n {
		return false
	}
	return b.bits[idx/8]&(1<<uint(idx%8)) != 0
}

// Count — количество установленных битов (полученных chunks). O(n).
// Хранится также в file_downloads.chunks_received как fast-path.
func (b *Bitmap) Count() int {
	c := 0
	for i := 0; i < b.n; i++ {
		if b.Get(i) {
			c++
		}
	}
	return c
}

// Complete — все chunks получены.
func (b *Bitmap) Complete() bool { return b.Count() == b.n }

// Missing возвращает первые up-to maxN индексов отсутствующих chunks.
// Используется pull-loop'ом для batch-запроса (запросить сразу 10).
//
// Внутри ищем contiguous-runs missing chunks — если есть подряд 5 missing,
// pull может попросить chunk_request(start=N, count=5) одним фреймом.
// Возвращаем pair (startIdx, count) для каждого ranges.
func (b *Bitmap) MissingRanges(maxRanges, maxCountPerRange int) []ChunkRange {
	if maxRanges <= 0 {
		maxRanges = 1
	}
	if maxCountPerRange <= 0 {
		maxCountPerRange = 1
	}
	out := make([]ChunkRange, 0, maxRanges)
	i := 0
	for i < b.n && len(out) < maxRanges {
		// Skip received
		for i < b.n && b.Get(i) {
			i++
		}
		if i >= b.n {
			break
		}
		// Found missing — extend run.
		start := i
		count := 0
		for i < b.n && !b.Get(i) && count < maxCountPerRange {
			count++
			i++
		}
		out = append(out, ChunkRange{Start: start, Count: count})
	}
	return out
}

// ChunkRange — диапазон [Start, Start+Count) для запроса.
type ChunkRange struct {
	Start int
	Count int
}
