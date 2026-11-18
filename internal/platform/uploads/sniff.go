package uploads

import "bytes"

// sniffSize is how much of a file the type check reads.
const sniffSize = 8 << 10

// matchesType checks a file's first bytes against its declared type,
// whatever its name says. It's a gate, not a parser: the service still
// rejects a file it can't read (a CSV in an encoding it doesn't take,
// a zip that isn't a workbook) when it processes it.
func matchesType(want ContentType, head []byte) bool {
	switch want {
	case CSV:
		// Text: no NUL and no control bytes but tab, newline, carriage
		// return and form feed. Encodings are 6.2's concern.
		for _, b := range head {
			if b < 0x20 && b != '\t' && b != '\n' && b != '\r' && b != '\f' || b == 0x7f {
				return false
			}
		}
		return true
	case XLSX:
		// An XLSX is a zip: its first local file header.
		return bytes.HasPrefix(head, []byte("PK\x03\x04"))
	default:
		return false
	}
}
