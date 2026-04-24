package unpack

import "bytes"

func Deobfuscate(data []byte) []byte {
	if len(data) >= 4 && bytes.Equal(data[:4], []byte{0x20, 0x00, 0x00, 0x00}) {
		data = data[4:]
	} else if len(data) >= 4 && bytes.Equal(data[:4], []byte{0x10, 0x00, 0x00, 0x00}) {
		data = data[4:]
		if len(data) >= 128 {
			header := make([]byte, 128)
			pattern := bytes.Repeat([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0x00, 0x00, 0x00}, 16)
			for i := 0; i < 128; i++ {
				header[i] = data[i] ^ pattern[i]
			}
			data = append(header, data[128:]...)
		}
	}
	return data
}
