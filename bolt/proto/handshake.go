package proto

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// Magic is the 4-byte Bolt protocol preamble sent by the client at the start
// of every connection.
const Magic = uint32(0x6060B017)

// ErrNoCommonVersion is returned by Negotiate when the client's offered
// versions do not include any version that this server supports.
var ErrNoCommonVersion = errors.New("bolt: no common protocol version")

// ErrBadMagic is returned by Negotiate when the client sends an incorrect
// magic preamble.
var ErrBadMagic = errors.New("bolt: bad magic preamble")

// Version represents a Bolt protocol version.
type Version struct {
	Major, Minor uint8
}

// SupportedVersions is the list of Bolt versions advertised by this server,
// ordered from highest to lowest preference.
var SupportedVersions = []Version{
	{5, 6}, {5, 5}, {5, 4}, {5, 3}, {5, 2}, {5, 1}, {5, 0},
	{4, 4},
}

// Negotiate performs the Bolt v5 handshake on conn.
//
// The client sends a 20-byte payload: 4-byte magic followed by four 4-byte
// version slots. Each slot encodes [major, minor, minor_range, 0] in big-endian
// order. minor_range > 0 means the client accepts versions in the range
// [minor-minor_range, minor], allowing a range offer in a single slot.
//
// Negotiate selects the highest version from SupportedVersions that falls
// within any offered range, writes back 4 bytes ([major, minor, 0, 0]), and
// returns the agreed Version. The context deadline, if set, governs I/O
// timeouts via conn.SetDeadline.
//
// Returns ErrBadMagic if the magic preamble is wrong, ErrNoCommonVersion if no
// version matches, or an I/O error if the connection fails mid-handshake.
//
//nolint:gocyclo // handshake has multiple sequential error checks + version-matching loops; splitting would obscure the protocol flow.
func Negotiate(ctx context.Context, conn net.Conn) (Version, error) {
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return Version{}, fmt.Errorf("bolt: SetDeadline: %w", err)
		}
		defer func() {
			// Clear deadline after handshake so subsequent I/O is not bounded.
			_ = conn.SetDeadline(time.Time{})
		}()
	}

	// Read 20 bytes: 4 magic + 4×4 version offers.
	var buf [20]byte
	if _, err := io.ReadFull(conn, buf[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return Version{}, io.ErrUnexpectedEOF
		}
		return Version{}, fmt.Errorf("bolt: read handshake: %w", err)
	}

	// Validate magic.
	magic := binary.BigEndian.Uint32(buf[:4])
	if magic != Magic {
		return Version{}, ErrBadMagic
	}

	// Parse four version slots.
	// Bolt wire format per slot: [major, minor, minor_range, 0] big-endian.
	// minor_range: the client accepts [minor - minor_range, minor].
	type versionOffer struct {
		major      uint8
		minor      uint8
		minorRange uint8
	}
	offers := make([]versionOffer, 0, 4)
	for i := 0; i < 4; i++ {
		slot := buf[4+i*4 : 4+i*4+4]
		major := slot[0]
		minor := slot[1]
		minorRange := slot[2]
		if major == 0 && minor == 0 {
			continue // zero slot = not offered
		}
		offers = append(offers, versionOffer{major: major, minor: minor, minorRange: minorRange})
	}

	// Select the highest supported version that matches any offer.
	// SupportedVersions is already ordered highest-first.
	for _, sv := range SupportedVersions {
		for _, o := range offers {
			if o.major != sv.Major {
				continue
			}
			// Guard against underflow when minorRange > minor.
			var minMinor uint8
			if o.minorRange <= o.minor {
				minMinor = o.minor - o.minorRange
			}
			if sv.Minor >= minMinor && sv.Minor <= o.minor {
				// Write back the agreed version: [major, minor, 0, 0].
				var resp [4]byte
				resp[0] = sv.Major
				resp[1] = sv.Minor
				if _, err := conn.Write(resp[:]); err != nil {
					return Version{}, fmt.Errorf("bolt: write version: %w", err)
				}
				return sv, nil
			}
		}
	}

	// No common version: write back [0, 0, 0, 0] to signal rejection.
	var zero [4]byte
	_, _ = conn.Write(zero[:]) // best-effort; ignore write error on rejection path
	return Version{}, ErrNoCommonVersion
}
