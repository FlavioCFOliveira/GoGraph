package server

import (
	"fmt"
	"sync/atomic"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
)

// bookmarkCounter is the monotonically increasing transaction counter used to
// generate bookmark strings. It is incremented atomically so NextBookmark is
// safe for concurrent use.
var bookmarkCounter atomic.Uint64

// NextBookmark generates a new bookmark string for a committed transaction.
// The format is "FB:kXXXXXX" where XXXXXX is a monotonically increasing
// counter expressed as a zero-padded 8-digit hexadecimal value.
//
// NextBookmark is safe for concurrent use.
func NextBookmark() string {
	n := bookmarkCounter.Add(1)
	return fmt.Sprintf("FB:k%08x", n)
}

// ExtractBookmarks returns the bookmark list from RUN/BEGIN extra metadata.
// It reads the "bookmarks" key, which may be a []packstream.Value of strings.
// Returns nil (not an error) when the key is absent or the value is not a list.
func ExtractBookmarks(extra map[string]packstream.Value) []string {
	v, ok := extra["bookmarks"]
	if !ok || v == nil {
		return nil
	}
	list, ok := v.([]packstream.Value)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, elem := range list {
		if s, ok := elem.(string); ok {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
