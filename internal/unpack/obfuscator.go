package unpack

import (
	"bytes"
	"io"
)

var deobfuscateHeaderPattern = []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0x00, 0x00, 0x00}

func Deobfuscate(data []byte) []byte {
	if len(data) >= 4 && bytes.Equal(data[:4], []byte{0x20, 0x00, 0x00, 0x00}) {
		data = data[4:]
	} else if len(data) >= 4 && bytes.Equal(data[:4], []byte{0x10, 0x00, 0x00, 0x00}) {
		data = data[4:]
		if len(data) >= 128 {
			for i := 0; i < 128; i++ {
				data[i] ^= deobfuscateHeaderPattern[i%len(deobfuscateHeaderPattern)]
			}
		}
	}
	return data
}

func DeobfuscateToWriter(dst io.Writer, src io.Reader) error {
	prefix := make([]byte, 4)
	n, err := io.ReadFull(src, prefix)
	if err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			_, writeErr := dst.Write(prefix[:n])
			return writeErr
		}
		return err
	}

	if bytes.Equal(prefix, []byte{0x20, 0x00, 0x00, 0x00}) {
		_, err = io.Copy(dst, src)
		return err
	}

	if bytes.Equal(prefix, []byte{0x10, 0x00, 0x00, 0x00}) {
		header := make([]byte, 128)
		n, err := io.ReadFull(src, header)
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				_, writeErr := dst.Write(header[:n])
				return writeErr
			}
			return err
		}
		for i := range header {
			header[i] ^= deobfuscateHeaderPattern[i%len(deobfuscateHeaderPattern)]
		}
		if _, err := dst.Write(header); err != nil {
			return err
		}
		_, err = io.Copy(dst, src)
		return err
	}

	if _, err := dst.Write(prefix); err != nil {
		return err
	}
	_, err = io.Copy(dst, src)
	return err
}
