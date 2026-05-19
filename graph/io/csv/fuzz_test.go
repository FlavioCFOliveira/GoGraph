package csv

import (
	"bytes"
	"testing"
)

// FuzzCSVReader feeds arbitrary bytes into the CSV edge-list parser.
// Seeded with a small valid CSV plus its header-on variant.
func FuzzCSVReader(f *testing.F) {
	f.Add([]byte("a,b,1\nb,c,2\n"))
	f.Add([]byte("src,dst,weight\nalice,bob,7\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("ReadInto panicked on input: %v", r)
			}
		}()
		_, _, _ = ReadInto(bytes.NewReader(data), Options{Delimiter: ',', HasHeader: false})
	})
}
