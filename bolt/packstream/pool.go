package packstream

import (
	"bufio"
	"bytes"
	"sync"

	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// EncodePool is a pool of Encoders backed by bytes.Buffer writers.
// It is safe for concurrent use, and its zero value is ready to use:
// [NewEncodePool] installs the pool's constructor eagerly, and [EncodePool.Get]
// falls back to constructing an Encoder when it is absent.
type EncodePool struct {
	p sync.Pool
}

// NewEncodePool creates an EncodePool.
func NewEncodePool() *EncodePool {
	ep := &EncodePool{}
	ep.p.New = func() any {
		buf := new(bytes.Buffer)
		return newEncoderFromBufio(bufio.NewWriter(buf))
	}
	return ep
}

// Get retrieves an Encoder from the pool, resetting it to write into dst.
// The caller must call Put when done.
func (ep *EncodePool) Get(dst *bytes.Buffer) *Encoder {
	metrics.IncCounter("bolt.pool.encoder.get", 1)
	// A zero-value EncodePool has a nil sync.Pool.New, so Get returns nil and an
	// unchecked assertion would panic. Construct on demand instead: the exported
	// zero value must not be a crash (#2708).
	enc, ok := ep.p.Get().(*Encoder)
	if !ok {
		enc = newEncoderFromBufio(bufio.NewWriter(new(bytes.Buffer)))
	}
	enc.Reset(dst)
	return enc
}

// Put returns enc to the pool for reuse.
func (ep *EncodePool) Put(enc *Encoder) {
	metrics.IncCounter("bolt.pool.encoder.put", 1)
	ep.p.Put(enc)
}

// DecodePool is a pool of Decoders backed by bytes.Reader readers.
// It is safe for concurrent use, and its zero value is ready to use:
// [NewDecodePool] installs the pool's constructor eagerly, and [DecodePool.Get]
// falls back to constructing a Decoder when it is absent.
type DecodePool struct {
	p sync.Pool
}

// NewDecodePool creates a DecodePool.
func NewDecodePool() *DecodePool {
	dp := &DecodePool{}
	dp.p.New = func() any {
		return newDecoderFromBufio(bufio.NewReader(bytes.NewReader(nil)))
	}
	return dp
}

// Get retrieves a Decoder from the pool, resetting it to read from src.
// The caller must call Put when done.
func (dp *DecodePool) Get(src *bytes.Reader) *Decoder {
	metrics.IncCounter("bolt.pool.decoder.get", 1)
	// A zero-value DecodePool has a nil sync.Pool.New, so Get returns nil and an
	// unchecked assertion would panic. Construct on demand instead: the exported
	// zero value must not be a crash (#2708).
	dec, ok := dp.p.Get().(*Decoder)
	if !ok {
		dec = newDecoderFromBufio(bufio.NewReader(bytes.NewReader(nil)))
	}
	dec.Reset(src)
	return dec
}

// Put returns dec to the pool for reuse.
func (dp *DecodePool) Put(dec *Decoder) {
	metrics.IncCounter("bolt.pool.decoder.put", 1)
	dp.p.Put(dec)
}
