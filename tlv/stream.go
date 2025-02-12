package tlv

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math"
)

// MaxRecordSize is the maximum size of a particular record that will be parsed
// by a stream decoder. This value is currently chosen to the be equal to the
// maximum message size permitted by BOLT 1, as no record should be bigger than
// an entire message.
const MaxRecordSize = 65535 // 65KB

// ErrStreamNotCanonical signals that a decoded stream does not contain records
// sorting by monotonically-increasing type.
var ErrStreamNotCanonical = errors.New("tlv stream is not canonical")

// ErrRecordTooLarge signals that a decoded record has a length that is too
// long to parse.
var ErrRecordTooLarge = errors.New("record is too large")

// ErrUnknownRequiredType is an error returned when decoding an unknown and even
// type from a Stream.
type ErrUnknownRequiredType Type

// Error returns a human-readable description of unknown required type.
func (t ErrUnknownRequiredType) Error() string {
	return fmt.Sprintf("unknown required type: %d", t)
}

// Stream defines a TLV stream that can be used for encoding or decoding a set
// of TLV Records.
type Stream struct {
	records []Record
	buf     [8]byte
}

// NewStream creates a new TLV Stream given an encoding codec, a decoding codec,
// and a set of known records.
func NewStream(records ...Record) (*Stream, error) {
	// Assert that the ordering of the Records is canonical and appear in
	// ascending order of type.
	var (
		min      Type
		overflow bool
	)
	for _, record := range records {
		if overflow || record.typ < min {
			return nil, ErrStreamNotCanonical
		}
		if record.encoder == nil {
			record.encoder = ENOP
		}
		if record.decoder == nil {
			record.decoder = DNOP
		}
		if record.typ == math.MaxUint64 {
			overflow = true
		}
		min = record.typ + 1
	}

	return &Stream{
		records: records,
	}, nil
}

// MustNewStream creates a new TLV Stream given an encoding codec, a decoding
// codec, and a set of known records. If an error is encountered in creating the
// stream, this method will panic instead of returning the error.
func MustNewStream(records ...Record) *Stream {
	stream, err := NewStream(records...)
	if err != nil {
		panic(err.Error())
	}
	return stream
}

// Encode writes a Stream to the passed io.Writer. Each of the Records known to
// the Stream is written in ascending order of their type so as to be canonical.
//
// The stream is constructed by concatenating the individual, serialized Records
// where each record has the following format:
//    [varint: type]
//    [varint: length]
//    [length: value]
//
// An error is returned if the io.Writer fails to accept bytes from the
// encoding, and nothing else. The ordering of the Records is asserted upon the
// creation of a Stream, and thus the output will be by definition canonical.
func (s *Stream) Encode(w io.Writer) error {
	// Iterate through all known records, if any, serializing each record's
	// type, length and value.
	for i := range s.records {
		rec := &s.records[i]

		// Write the record's type as a varint.
		err := WriteVarInt(w, uint64(rec.typ), &s.buf)
		if err != nil {
			return err
		}

		// Write the record's length as a varint.
		err = WriteVarInt(w, rec.Size(), &s.buf)
		if err != nil {
			return err
		}

		// Encode the current record's value using the stream's codec.
		err = rec.encoder(w, rec.value, &s.buf)
		if err != nil {
			return err
		}
	}

	return nil
}

// Decode deserializes TLV Stream from the passed io.Reader. The Stream will
// inspect each record that is parsed and check to see if it has a corresponding
// Record to facilitate deserialization of that field. If the record is unknown,
// the Stream will discard the record's bytes and proceed to the subsequent
// record.
//
// Each record has the following format:
//    [varint: type]
//    [varint: length]
//    [length: value]
//
// A series of (possibly zero) records are concatenated into a stream, this
// example contains two records:
//
//    (t: 0x01, l: 0x04, v: 0xff, 0xff, 0xff, 0xff)
//    (t: 0x02, l: 0x01, v: 0x01)
//
// This method asserts that the byte stream is canonical, namely that each
// record is unique and that all records are sorted in ascending order. An
// ErrNotCanonicalStream error is returned if the encoded TLV stream is not.
//
// We permit an io.EOF error only when reading the type byte which signals that
// the last record was read cleanly and we should stop parsing. All other io.EOF
// or io.ErrUnexpectedEOF errors are returned.
func (s *Stream) Decode(r io.Reader) error {
	_, err := s.decode(r, nil)
	return err
}

// DecodeWithParsedTypes is identical to Decode, but if successful, returns a
// TypeSet containing the types of all records that were decoded or ignored from
// the stream.
func (s *Stream) DecodeWithParsedTypes(r io.Reader) (TypeSet, error) {
	return s.decode(r, make(TypeSet))
}

// decode is a helper function that performs the basis of stream decoding. If
// the caller needs the set of parsed types, it must provide an initialized
// parsedTypes, otherwise the returned TypeSet will be nil.
func (s *Stream) decode(r io.Reader, parsedTypes TypeSet) (TypeSet, error) {
	var (
		typ       Type
		min       Type
		firstFail *Type
		recordIdx int
		overflow  bool
	)

	// Iterate through all possible type identifiers. As types are read from
	// the io.Reader, min will skip forward to the last read type.
	for {
		// Read the next varint type.
		t, err := ReadVarInt(r, &s.buf)
		switch {

		// We'll silence an EOF when zero bytes remain, meaning the
		// stream was cleanly encoded.
		case err == io.EOF:
			if firstFail == nil {
				return parsedTypes, nil
			}
			return parsedTypes, ErrUnknownRequiredType(*firstFail)

		// Other unexpected errors.
		case err != nil:
			return nil, err
		}

		typ = Type(t)

		// Assert that this type is greater than any previously read.
		// If we've already overflowed and we parsed another type, the
		// stream is not canonical. This check prevents us from accepts
		// encodings that have duplicate records or from accepting an
		// unsorted series.
		if overflow || typ < min {
			return nil, ErrStreamNotCanonical
		}

		// Read the varint length.
		length, err := ReadVarInt(r, &s.buf)
		switch {

		// We'll convert any EOFs to ErrUnexpectedEOF, since this
		// results in an invalid record.
		case err == io.EOF:
			return nil, io.ErrUnexpectedEOF

		// Other unexpected errors.
		case err != nil:
			return nil, err
		}

		// Place a soft limit on the size of a sane record, which
		// prevents malicious encoders from causing us to allocate an
		// unbounded amount of memory when decoding variable-sized
		// fields.
		if length > MaxRecordSize {
			return nil, ErrRecordTooLarge
		}

		// Search the records known to the stream for this type. We'll
		// begin the search and recordIdx and walk forward until we find
		// it or the next record's type is larger.
		rec, newIdx, ok := s.getRecord(typ, recordIdx)
		switch {

		// We know of this record type, proceed to decode the value.
		// This method asserts that length bytes are read in the
		// process, and returns an error if the number of bytes is not
		// exactly length.
		case ok:
			err := rec.decoder(r, rec.value, &s.buf, length)
			switch {

			// We'll convert any EOFs to ErrUnexpectedEOF, since this
			// results in an invalid record.
			case err == io.EOF:
				return nil, io.ErrUnexpectedEOF

			// Other unexpected errors.
			case err != nil:
				return nil, err
			}

		// This record type is unknown to the stream, fail if the type
		// is even meaning that we are required to understand it.
		case typ%2 == 0:
			// We'll fail immediately in the case that we aren't
			// tracking the set of parsed types.
			if parsedTypes == nil {
				return nil, ErrUnknownRequiredType(typ)
			}

			// Otherwise, we'll track the first such failure and
			// allow parsing to continue. If no other types of
			// errors are encountered, the first failure will be
			// returned as an ErrUnknownRequiredType so that the
			// full set of included types can be returned.
			if firstFail == nil {
				failTyp := typ
				firstFail = &failTyp
			}

			// With the failure type recorded, we'll simply discard
			// the remainder of the record as if it were optional.
			// The first failure will be returned after reaching the
			// stopping condition.
			fallthrough

		// Otherwise, the record type is unknown and is odd, discard the
		// number of bytes specified by length.
		default:
			_, err := io.CopyN(ioutil.Discard, r, int64(length))
			switch {

			// We'll convert any EOFs to ErrUnexpectedEOF, since this
			// results in an invalid record.
			case err == io.EOF:
				return nil, io.ErrUnexpectedEOF

			// Other unexpected errors.
			case err != nil:
				return nil, err
			}
		}

		// Record the successfully decoded or ignored type if the
		// caller provided an initialized TypeSet.
		if parsedTypes != nil {
			parsedTypes[typ] = struct{}{}
		}

		// Update our record index so that we can begin our next search
		// from where we left off.
		recordIdx = newIdx

		// If we've parsed the largest possible type, the next loop will
		// overflow back to zero. However, we need to attempt parsing
		// the next type to ensure that the stream is empty.
		if typ == math.MaxUint64 {
			overflow = true
		}

		// Finally, set our lower bound on the next accepted type.
		min = typ + 1
	}
}

// getRecord searches for a record matching typ known to the stream. The boolean
// return value indicates whether the record is known to the stream. The integer
// return value carries the index from where getRecord should be invoked on the
// subsequent call. The first call to getRecord should always use an idx of 0.
func (s *Stream) getRecord(typ Type, idx int) (Record, int, bool) {
	for idx < len(s.records) {
		record := s.records[idx]
		switch {

		// Found target record, return it to the caller. The next index
		// returned points to the immediately following record.
		case record.typ == typ:
			return record, idx + 1, true

		// This record's type is lower than the target. Advance our
		// index and continue to the next record which will have a
		// strictly higher type.
		case record.typ < typ:
			idx++
			continue

		// This record's type is larger than the target, hence we have
		// no record matching the current type. Return the current index
		// so that we can start our search from here when processing the
		// next tlv record.
		default:
			return Record{}, idx, false
		}
	}

	// All known records are exhausted.
	return Record{}, idx, false
}
