// Copyright 2014 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package engine

import (
	"encoding/binary"
	"fmt"

	"github.com/cockroachdb/cockroach/pkg/storage/engine/enginepb"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/pkg/errors"
)

// BatchType represents the type of an entry in an encoded RocksDB batch.
type BatchType byte

// These constants come from rocksdb/db/dbformat.h.
const (
	BatchTypeDeletion BatchType = 0x0
	BatchTypeValue    BatchType = 0x1
	BatchTypeMerge    BatchType = 0x2
	BatchTypeLogData  BatchType = 0x3
	// BatchTypeColumnFamilyDeletion       BatchType = 0x4
	// BatchTypeColumnFamilyValue          BatchType = 0x5
	// BatchTypeColumnFamilyMerge          BatchType = 0x6
	BatchTypeSingleDeletion BatchType = 0x7
	// BatchTypeColumnFamilySingleDeletion BatchType = 0x8
	// BatchTypeBeginPrepareXID            BatchType = 0x9
	// BatchTypeEndPrepareXID              BatchType = 0xA
	// BatchTypeCommitXID                  BatchType = 0xB
	// BatchTypeRollbackXID                BatchType = 0xC
	// BatchTypeNoop                       BatchType = 0xD
	// BatchTypeColumnFamilyRangeDeletion  BatchType = 0xE
	BatchTypeRangeDeletion BatchType = 0xF
	// BatchTypeColumnFamilyBlobIndex      BatchType = 0x10
	// BatchTypeBlobIndex                  BatchType = 0x11
	// BatchMaxValue                       BatchType = 0x7F
)

const (
	// The batch header is composed of an 8-byte sequence number (all zeroes) and
	// 4-byte count of the number of entries in the batch.
	headerSize           int = 12
	countPos                 = 8
	initialBatchSize         = 1 << 10 // 1 KB
	maxRetainedBatchSize     = 1 << 20 // 1 MB
	maxVarintLen32           = 5
)

// RocksDBBatchBuilder is used to construct the RocksDB batch representation.
// From the RocksDB code, the representation of a batch is:
//
//   WriteBatch::rep_ :=
//      sequence: fixed64
//      count: fixed32
//      data: record[count]
//   record :=
//      kTypeValue varstring varstring
//      kTypeDeletion varstring
//      [...] (see BatchType)
//   varstring :=
//      len: varint32
//      data: uint8[len]
//
// The RocksDBBatchBuilder code currently only supports kTypeValue
// (BatchTypeValue), kTypeDeletion (BatchTypeDeletion), kTypeMerge
// (BatchTypeMerge), and kTypeSingleDeletion (BatchTypeSingleDeletion)
// operations. Before a batch is written to the RocksDB write-ahead-log,
// the sequence number is 0. The "fixed32" format is little endian.
//
// The keys encoded into the batch are MVCC keys: a string key with a timestamp
// suffix. MVCC keys are encoded as:
//
//   <key>[<wall_time>[<logical>]]<#timestamp-bytes>
//
// The <wall_time> and <logical> portions of the key are encoded as 64 and
// 32-bit big-endian integers. A custom RocksDB comparator is used to maintain
// the desired ordering as these keys do not sort lexicographically correctly.
// Note that the encoding of these keys needs to match up with the encoding in
// rocksdb/db.cc:EncodeKey().
type RocksDBBatchBuilder struct {
	repr    []byte
	count   int
	logData bool
}

func (b *RocksDBBatchBuilder) maybeInit() {
	if b.repr == nil {
		b.repr = make([]byte, headerSize, initialBatchSize)
	}
}

func (b *RocksDBBatchBuilder) reset() {
	if b.repr != nil {
		if cap(b.repr) > maxRetainedBatchSize {
			// If the capacity of the buffer is larger than our maximum
			// retention size, don't re-use it. Let it be GC-ed instead.
			// This prevents the memory from an unusually large batch from
			// being held on to indefinitely.
			b.repr = nil
		} else {
			// Otherwise, reset the buffer for re-use.
			b.repr = b.repr[:headerSize]
		}
	}
	b.count = 0
	b.logData = false
}

// Finish returns the constructed batch representation. After calling Finish,
// the builder may be used to construct another batch, but the returned []byte
// is only valid until the next builder method is called.
func (b *RocksDBBatchBuilder) Finish() []byte {
	repr := b.getRepr()
	b.repr = b.repr[:headerSize]
	b.count = 0
	b.logData = false
	return repr
}

// Len returns the number of bytes currently in the under construction repr.
func (b *RocksDBBatchBuilder) Len() int {
	return len(b.repr)
}

var _ = (*RocksDBBatchBuilder).Len

// getRepr constructs the batch representation and returns it.
func (b *RocksDBBatchBuilder) getRepr() []byte {
	b.maybeInit()
	buf := b.repr[countPos:headerSize]
	v := uint32(b.count)
	buf[0] = byte(v)
	buf[1] = byte(v >> 8)
	buf[2] = byte(v >> 16)
	buf[3] = byte(v >> 24)
	return b.repr
}

func (b *RocksDBBatchBuilder) grow(n int) {
	newSize := len(b.repr) + n
	if newSize > cap(b.repr) {
		newCap := 2 * cap(b.repr)
		for newCap < newSize {
			newCap *= 2
		}
		newRepr := make([]byte, len(b.repr), newCap)
		copy(newRepr, b.repr)
		b.repr = newRepr
	}
	b.repr = b.repr[:newSize]
}

func putUvarint32(buf []byte, x uint32) int {
	i := 0
	for x >= 0x80 {
		buf[i] = byte(x) | 0x80
		x >>= 7
		i++
	}
	buf[i] = byte(x)
	return i + 1
}

func putUint32(b []byte, v uint32) {
	b[0] = byte(v >> 24)
	b[1] = byte(v >> 16)
	b[2] = byte(v >> 8)
	b[3] = byte(v)
}

func putUint64(b []byte, v uint64) {
	b[0] = byte(v >> 56)
	b[1] = byte(v >> 48)
	b[2] = byte(v >> 40)
	b[3] = byte(v >> 32)
	b[4] = byte(v >> 24)
	b[5] = byte(v >> 16)
	b[6] = byte(v >> 8)
	b[7] = byte(v)
}

// encodeKey encodes an MVCC key into the batch, reserving extra bytes in
// b.repr for use in encoding a value as well. This encoding must match with
// the encoding in engine/db.cc:EncodeKey().
func (b *RocksDBBatchBuilder) encodeKey(key MVCCKey, extra int) {
	length := 1 + len(key.Key)
	timestampLength := 0
	if key.Timestamp != (hlc.Timestamp{}) {
		timestampLength = 1 + 8
		if key.Timestamp.Logical != 0 {
			timestampLength += 4
		}
	}
	length += timestampLength

	pos := 1 + len(b.repr)
	b.grow(1 + maxVarintLen32 + length + extra)
	n := putUvarint32(b.repr[pos:], uint32(length))
	b.repr = b.repr[:len(b.repr)-(maxVarintLen32-n)]
	pos += n
	copy(b.repr[pos:], key.Key)
	if timestampLength > 0 {
		pos += len(key.Key)
		b.repr[pos] = 0
		pos++
		putUint64(b.repr[pos:], uint64(key.Timestamp.WallTime))
		if key.Timestamp.Logical != 0 {
			pos += 8
			putUint32(b.repr[pos:], uint32(key.Timestamp.Logical))
		}
	}
	b.repr[len(b.repr)-1-extra] = byte(timestampLength)
}

func (b *RocksDBBatchBuilder) encodeKeyValue(key MVCCKey, value []byte, tag BatchType) {
	b.maybeInit()
	b.count++

	l := uint32(len(value))
	extra := int(l) + maxVarintLen32

	pos := len(b.repr)
	b.encodeKey(key, extra)
	b.repr[pos] = byte(tag)

	pos = len(b.repr) - extra
	n := putUvarint32(b.repr[pos:], l)
	b.repr = b.repr[:len(b.repr)-(maxVarintLen32-n)]
	copy(b.repr[pos+n:], value)
}

// Put sets the given key to the value provided.
//
// It is safe to modify the contents of the arguments after Put returns.
func (b *RocksDBBatchBuilder) Put(key MVCCKey, value []byte) {
	b.encodeKeyValue(key, value, BatchTypeValue)
}

// Merge is a high-performance write operation used for values which are
// accumulated over several writes. Multiple values can be merged sequentially
// into a single key; a subsequent read will return a "merged" value which is
// computed from the original merged values.
//
// It is safe to modify the contents of the arguments after Merge returns.
func (b *RocksDBBatchBuilder) Merge(key MVCCKey, value []byte) {
	b.encodeKeyValue(key, value, BatchTypeMerge)
}

// Clear removes the item from the db with the given key.
//
// It is safe to modify the contents of the arguments after Clear returns.
func (b *RocksDBBatchBuilder) Clear(key MVCCKey) {
	b.maybeInit()
	b.count++
	pos := len(b.repr)
	b.encodeKey(key, 0)
	b.repr[pos] = byte(BatchTypeDeletion)
}

// SingleClear removes the most recent item from the db with the given key.
//
// It is safe to modify the contents of the arguments after SingleClear returns.
func (b *RocksDBBatchBuilder) SingleClear(key MVCCKey) {
	b.maybeInit()
	b.count++
	pos := len(b.repr)
	b.encodeKey(key, 0)
	b.repr[pos] = byte(BatchTypeSingleDeletion)
}

// LogData adds a blob of log data to the batch. It will be written to the WAL,
// but otherwise uninterpreted by RocksDB.
//
// It is safe to modify the contents of the arguments after LogData returns.
func (b *RocksDBBatchBuilder) LogData(data []byte) {
	b.maybeInit()
	b.logData = true
	pos := len(b.repr)
	b.grow(1 + maxVarintLen32 + len(data))
	b.repr[pos] = byte(BatchTypeLogData)
	pos++
	n := putUvarint32(b.repr[pos:], uint32(len(data)))
	b.repr = b.repr[:len(b.repr)-(maxVarintLen32-n)]
	pos += n
	copy(b.repr[pos:], data)
}

// ApplyRepr applies the mutations in repr to the current batch.
//
// It is safe to modify the contents of the arguments after ApplyRepr
// returns.
func (b *RocksDBBatchBuilder) ApplyRepr(repr []byte) error {
	if len(repr) < headerSize {
		return errors.Errorf("batch repr too small: %d < %d", len(repr), headerSize)
	}
	b.maybeInit()
	pos := len(b.repr)
	data := repr[headerSize:]
	b.grow(len(data))
	copy(b.repr[pos:], data)
	b.count += int(binary.LittleEndian.Uint32(repr[countPos:headerSize]))
	return nil
}

// EncodeKey encodes an engine.MVCC key into the RocksDB representation. This
// encoding must match with the encoding in engine/db.cc:EncodeKey().
func EncodeKey(key MVCCKey) []byte {
	return EncodeKeyToBuf(nil, key)
}

// EncodeKeyToBuf encodes an engine.MVCC key into the RocksDB representation.
// This encoding must match with the encoding in engine/db.cc:EncodeKey().
func EncodeKeyToBuf(buf []byte, key MVCCKey) []byte {
	// TODO(dan): Unify this with (*RocksDBBatchBuilder).encodeKey.

	const (
		timestampSentinelLen      = 1
		walltimeEncodedLen        = 8
		logicalEncodedLen         = 4
		timestampEncodedLengthLen = 1
	)

	timestampLength := 0
	if key.Timestamp != (hlc.Timestamp{}) {
		timestampLength = timestampSentinelLen + walltimeEncodedLen
		if key.Timestamp.Logical != 0 {
			timestampLength += logicalEncodedLen
		}
	}

	sz := len(key.Key) + timestampLength + timestampEncodedLengthLen
	if cap(buf) < sz {
		buf = make([]byte, sz)
	} else {
		buf = buf[:sz]
	}

	copy(buf, key.Key)

	pos := len(key.Key)
	if timestampLength > 0 {
		buf[pos] = 0
		pos += timestampSentinelLen
		putUint64(buf[pos:], uint64(key.Timestamp.WallTime))
		pos += walltimeEncodedLen
		if key.Timestamp.Logical != 0 {
			putUint32(buf[pos:], uint32(key.Timestamp.Logical))
			pos += logicalEncodedLen
		}
	}
	buf[len(buf)-1] = byte(timestampLength)

	return buf
}

// DecodeMVCCKey decodes an engine.MVCCKey from its serialized representation. This
// decoding must match engine/db.cc:DecodeKey().
func DecodeMVCCKey(encodedKey []byte) (MVCCKey, error) {
	k, ts, err := enginepb.DecodeKey(encodedKey)
	return MVCCKey{k, ts}, err
}

// Decode the header of RocksDB batch repr, returning both the count of the
// entries in the batch and the suffix of data remaining in the batch.
func rocksDBBatchDecodeHeader(repr []byte) (count int, orepr []byte, err error) {
	if len(repr) < headerSize {
		return 0, nil, errors.Errorf("batch repr too small: %d < %d", len(repr), headerSize)
	}
	seq := binary.LittleEndian.Uint64(repr[:countPos])
	if seq != 0 {
		return 0, nil, errors.Errorf("bad sequence: expected 0, but found %d", seq)
	}
	count = int(binary.LittleEndian.Uint32(repr[countPos:headerSize]))
	return count, repr[headerSize:], nil
}

// Decode a RocksDB batch repr variable length string, returning both the
// string and the suffix of data remaining in the batch.
func rocksDBBatchVarString(repr []byte) (s []byte, orepr []byte, err error) {
	v, n := binary.Uvarint(repr)
	if n <= 0 {
		return nil, nil, fmt.Errorf("unable to decode uvarint")
	}
	repr = repr[n:]
	if v == 0 {
		return nil, repr, nil
	}
	if v > uint64(len(repr)) {
		return nil, nil, fmt.Errorf("malformed varstring, expected %d bytes, but only %d remaining",
			v, len(repr))
	}
	return repr[:v], repr[v:], nil
}

// RocksDBBatchReader is used to iterate the entries in a RocksDB batch
// representation.
//
// Example:
// r, err := NewRocksDBBatchReader(...)
// if err != nil {
//   return err
// }
// for r.Next() {
// 	 switch r.BatchType() {
// 	 case BatchTypeDeletion:
// 	   fmt.Printf("delete(%x)", r.Key())
// 	 case BatchTypeValue:
// 	   fmt.Printf("put(%x,%x)", r.Key(), r.Value())
// 	 case BatchTypeMerge:
// 	   fmt.Printf("merge(%x,%x)", r.Key(), r.Value())
//   case BatchTypeSingleDeletion:
// 	   fmt.Printf("single_delete(%x)", r.Key())
//   case BatchTypeRangeDeletion:
// 	   fmt.Printf("delete_range(%x,%x)", r.Key(), r.Value())
// 	 }
// }
// if err := r.Error(); err != nil {
//   return err
// }
type RocksDBBatchReader struct {
	repr []byte

	// The error encountered during iterator, if any
	err error

	// The total number of entries, decoded from the batch header
	count int

	// The following all represent the current entry and are updated by Next.
	// `value` is not applicable for BatchTypeDeletion or BatchTypeSingleDeletion.
	// `value` indicates the end key for BatchTypeRangeDeletion.
	offset int
	typ    BatchType
	key    []byte
	value  []byte
}

// NewRocksDBBatchReader creates a RocksDBBatchReader from the given repr and
// verifies the header.
func NewRocksDBBatchReader(repr []byte) (*RocksDBBatchReader, error) {
	count, repr, err := rocksDBBatchDecodeHeader(repr)
	if err != nil {
		return nil, err
	}
	// Set offset to -1 so the first call to Next will increment it to 0.
	return &RocksDBBatchReader{repr: repr, count: count, offset: -1}, nil
}

// Count returns the declared number of entries in the batch.
func (r *RocksDBBatchReader) Count() int {
	return r.count
}

// Error returns the error, if any, which the iterator encountered.
func (r *RocksDBBatchReader) Error() error {
	return r.err
}

// BatchType returns the type of the current batch entry.
func (r *RocksDBBatchReader) BatchType() BatchType {
	return r.typ
}

// Key returns the key of the current batch entry.
func (r *RocksDBBatchReader) Key() []byte {
	return r.key
}

func decodeMVCCKey(k []byte) (MVCCKey, error) {
	k, ts, err := enginepb.DecodeKey(k)
	return MVCCKey{k, ts}, err
}

// MVCCKey returns the MVCC key of the current batch entry.
func (r *RocksDBBatchReader) MVCCKey() (MVCCKey, error) {
	return decodeMVCCKey(r.Key())
}

// Value returns the value of the current batch entry. Value panics if the
// BatchType is BatchTypeDeleted.
func (r *RocksDBBatchReader) Value() []byte {
	if r.typ == BatchTypeDeletion || r.typ == BatchTypeSingleDeletion {
		panic("cannot call Value on a deletion entry")
	}
	return r.value
}

// MVCCEndKey returns the MVCC end key of the current batch entry.
func (r *RocksDBBatchReader) MVCCEndKey() (MVCCKey, error) {
	if r.typ != BatchTypeRangeDeletion {
		panic("cannot only call Value on a range deletion entry")
	}
	return decodeMVCCKey(r.Value())
}

// Next advances to the next entry in the batch, returning false when the batch
// is empty.
func (r *RocksDBBatchReader) Next() bool {
	if r.err != nil {
		return false
	}

	r.offset++
	if len(r.repr) == 0 {
		if r.offset < r.count {
			r.err = errors.Errorf("invalid batch: expected %d entries but found %d", r.count, r.offset)
		}
		return false
	}

	r.typ = BatchType(r.repr[0])
	r.repr = r.repr[1:]
	switch r.typ {
	case BatchTypeDeletion, BatchTypeSingleDeletion:
		if r.key, r.err = r.varstring(); r.err != nil {
			return false
		}
	case BatchTypeValue, BatchTypeMerge, BatchTypeRangeDeletion:
		if r.key, r.err = r.varstring(); r.err != nil {
			return false
		}
		if r.value, r.err = r.varstring(); r.err != nil {
			return false
		}
	default:
		r.err = errors.Errorf("unexpected type %d", r.typ)
		return false
	}
	return true
}

func (r *RocksDBBatchReader) varstring() ([]byte, error) {
	var s []byte
	var err error
	s, r.repr, err = rocksDBBatchVarString(r.repr)
	return s, err
}

// RocksDBBatchCount provides an efficient way to get the count of mutations
// in a RocksDB Batch representation.
func RocksDBBatchCount(repr []byte) (int, error) {
	if len(repr) < headerSize {
		return 0, errors.Errorf("batch repr too small: %d < %d", len(repr), headerSize)
	}
	return int(binary.LittleEndian.Uint32(repr[countPos:headerSize])), nil
}
