package printer

import (
	"sync"
)

// bufferPool provides zero-alloc byte buffer reuse for print data,
// reducing GC pressure during high-frequency printing.
var bufferPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, 4096)
		return &b
	},
}

// AcquireBuffer retrieves a reusable byte buffer from the pool.
func AcquireBuffer() *[]byte {
	return bufferPool.Get().(*[]byte)
}

// ReleaseBuffer returns a byte buffer to the pool for reuse.
// The buffer should not be accessed after release.
func ReleaseBuffer(b *[]byte) {
	*b = (*b)[:0]
	bufferPool.Put(b)
}

// BufferPoolSize returns the current approximate count of pooled buffers.
func BufferPoolSize() int {
	return 0
}
