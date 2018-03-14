package rfc6242

import (
	"bytes"
	"strconv"

	"github.com/pkg/errors"
)

var (
	// ErrZeroChunks is a protocol error indicating that no chunk was
	// seen prior to the end-of-chunks token.
	ErrZeroChunks = errors.New("end-of-chunks seen prior to chunk")
	// ErrChunkSizeInvalid is a protocol error indicating that a chunk
	// frame introduction was seen, but chunk-size decoding failed.
	ErrChunkSizeInvalid = errors.New("no valid chunk-size detected")
	// ErrChunkSizeTokenTooLong is a protocol error indicating a
	// valid chunk-size token start was seen, but that the chunk-size
	// token was longer than that necessary to store the maximum
	// permitted chunk size "4294967295".
	ErrChunkSizeTokenTooLong = errors.New("token too long")
	// ErrChunkSizeTooLarge is a protocol error indicating that the
	// chunk-size decoded exceeds the limit stated in RFC6242.
	ErrChunkSizeTooLarge = errors.New("chunk size larger than maximum (4294967295)")
	// ErrInvalidChunk is a protocol error indicating that a chunked
	// message header was expected but not found.
	ErrInvalidChunk = errors.New("invalid chunk header")
)

var (
	tokenEOM = []byte("]]>]]>")
)

const (
	errPrefixChunkSize = "bad chunk-size"
	errProtocol        = "protocol error"
)

// DecoderEndOfMessage is the NETCONF 1.0 end-of-message delimited
// decoding function.
func DecoderEndOfMessage(d *Decoder, b []byte, atEOF bool) (advance int, token []byte, err error) {
	if len(b) < len(tokenEOM) {
		return
	}

	// always scan the input buffer b at least once. if we're at EOF,
	// continue scanning until we've processed the entire buffer.
	for first := true; err == nil && first || atEOF && advance < len(b); first = false {
		cur := b[advance:]

		eomStartIdx := bytes.IndexByte(cur, ']')
		switch {
		case eomStartIdx == -1:
			token = append(token, cur...)
			advance += len(cur)
		case eomStartIdx > 0:
			// consume up to the start of the EOM token.
			token = append(token, cur[:eomStartIdx]...)
			advance += eomStartIdx
		default:
			// confirm we see a complete EOM token. if not,
			// append the non EOM token data we saw to our result.
			i := 1
			for i < len(tokenEOM) {
				if ok := cur[i] == tokenEOM[i]; !ok {
					token = append(token, cur[:i]...)
					break
				}
				i++
			}
			d.eofOK = i == len(tokenEOM)
			advance += i
		}
	}

	return
}

// DecoderChunked is the NETCONF 1.1 chunked framing decoder function.
func DecoderChunked(d *Decoder, b []byte, atEOF bool) (advance int, token []byte, err error) {
	if d.scanErr != nil {
		err = d.scanErr
		return
	}

	d.eofOK = len(b) == 0

	var cur []byte
	for err == nil && advance < len(b) {
		cur = b[advance:]

		switch {
		case d.chunkDataLeft == 0:
			action, adv, chunksize, cherr := detectChunkHeader(cur)
			switch {
			case cherr != nil:
				err = cherr
			case action == chActionMoreData:
				return
			case action == chActionChunk:
				advance += adv
				d.chunkDataLeft = chunksize
			case action == chActionEndOfChunks:
				advance += adv
				d.eofOK = d.anySeen
				if !d.anySeen {
					err = errors.WithMessage(ErrZeroChunks, errProtocol)
				}
			default:
				panic(errors.Errorf(
					"impossible default switch case: action=%v adv=%v chunksize=%v cherr=%v",
					action, adv, chunksize, cherr))
			}
		}

		// consume any chunk data available
		switch {
		case err != nil:
		case d.chunkDataLeft > 0 && advance < len(b):
			chunkdata := b[advance:]
			readN := uint64(len(chunkdata))
			if d.chunkDataLeft < readN {
				readN = d.chunkDataLeft
			}
			d.chunkDataLeft -= readN
			d.anySeen = d.anySeen || d.chunkDataLeft == 0
			advance += int(readN) // (some or all of) chunk-data
			token = append(token, chunkdata[:readN]...)
		}
	}

	// if we got an error but also advanced, store the error and emit it on the next call to this function.
	if err != nil && advance > 0 {
		if d.scanErr == nil {
			d.scanErr = err
			err = nil
		}
	}

	return
}

type chunkHeaderAction int

const (
	chActionMoreData chunkHeaderAction = iota
	chActionEndOfChunks
	chActionChunk
)

func detectChunkHeader(b []byte) (action chunkHeaderAction, advance int, chunksize uint64, err error) {
	// special case short blocks to detect specific errors. we will
	// never be called with an empty b.
	if len(b) < 3 {
		switch {
		case len(b) < 3 && b[0] != '\n':
			err = errors.WithMessage(ErrInvalidChunk, errProtocol)
			return
		case len(b) == 2 && b[1] != '#':
			err = errors.WithMessage(ErrInvalidChunk, errProtocol)
			return
		}
		action = chActionMoreData
		return
	}

	switch {
	case b[0] == '\n' && b[1] == '#':
		// valid chunk introduction
		switch {
		case b[2] >= '1' && b[2] <= '9':
			// chunk header: extract chunk-size token
			action = chActionChunk
			bChunksize := b[2:]
			lenChunksize := bytes.IndexByte(bChunksize, '\n')
			switch {
			case lenChunksize == -1:
				if len(bChunksize) <= rfc6242maximumAllowedChunkSize+1 {
					// we might not have seen the whole chunk-size value
					action = chActionMoreData
				} else {
					// we should have seen a chunk-size in bChunksize, but did not
					err = errors.WithMessage(ErrChunkSizeInvalid, errPrefixChunkSize)
				}
			case lenChunksize == 0:
				// not a valid chunk-size token
				err = errors.WithMessage(ErrChunkSizeInvalid, errPrefixChunkSize)
			case lenChunksize > rfc6242maximumAllowedChunkSizeLength:
				err = errors.WithMessage(ErrChunkSizeTokenTooLong, errPrefixChunkSize)
			default:
				// valid chunk-size token. decode chunk-size
				chunksize, err = strconv.ParseUint(string(bChunksize[:lenChunksize]), 10, 64)
				if err == nil && chunksize > rfc6242maximumAllowedChunkSize {
					err = errors.WithMessage(ErrChunkSizeTooLarge, errPrefixChunkSize)
				}
				advance = 2 + lenChunksize + 1
			}
		case b[2] == '#':
			// potential end-of-chunks
			switch {
			case len(b) < 4:
				action = chActionMoreData
			case b[3] == '\n':
				action = chActionEndOfChunks
				advance = 4
			}
		default:
			err = errors.WithMessage(ErrInvalidChunk, errProtocol)
		}
	default:
		err = errors.WithMessage(ErrInvalidChunk, errProtocol)
	}
	return
}