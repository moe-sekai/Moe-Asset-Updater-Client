package unpack

import (
	"bytes"
	"testing"
)

func TestDeobfuscateToWriterMatchesSliceImplementation(t *testing.T) {
	cases := map[string][]byte{
		"plain":  {0x01, 0x02, 0x03, 0x04, 0x05},
		"drop20": append([]byte{0x20, 0x00, 0x00, 0x00}, bytes.Repeat([]byte{0x11}, 256)...),
		"xor10":  append([]byte{0x10, 0x00, 0x00, 0x00}, bytes.Repeat([]byte{0x55}, 256)...),
		"short":  {0x10, 0x00, 0x00, 0x00, 0x01, 0x02},
	}

	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			expected := Deobfuscate(append([]byte(nil), input...))
			var out bytes.Buffer
			if err := DeobfuscateToWriter(&out, bytes.NewReader(input)); err != nil {
				t.Fatalf("DeobfuscateToWriter failed: %v", err)
			}
			if !bytes.Equal(out.Bytes(), expected) {
				t.Fatalf("stream output mismatch: got %x want %x", out.Bytes(), expected)
			}
		})
	}
}
