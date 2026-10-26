package backend

import (
	"io"
	"strings"
	"testing"
)

func TestBoundedOutputReaderFromHonorsLimit(t *testing.T) {
	for _, value := range []string{"12345678", "123456789"} {
		output := &boundedOutput{limit: 8}
		// A pipe copy uses ReaderFrom. Hide the source's WriterTo fast path so
		// this test exercises the same destination method used by os/exec.
		_, err := io.Copy(output, struct{ io.Reader }{strings.NewReader(value)})
		if len(value) > output.limit {
			if err == nil || !output.exceeded || output.Len() > output.limit {
				t.Fatalf("reader fast path bypassed capture bound: bytes=%d exceeded=%v error=%v", output.Len(), output.exceeded, err)
			}
		} else if err != nil || output.String() != value || output.exceeded {
			t.Fatalf("exact capture boundary changed: output=%q exceeded=%v error=%v", output.String(), output.exceeded, err)
		}
	}
}
