package archive

import (
	"encoding/hex"
	"strings"
	"time"
	"unicode/utf16"
)

type decodedPDU struct {
	sender    string
	timestamp *time.Time
	text      string
	dcs       byte
	ref       int
	total     int
	sequence  int
	decoded   bool
}

// decodePDU interprets incoming SMS-DELIVER TPDUs. Unsupported payloads
// stay in the archive as raw PDU and are never selected for deletion.
func decodePDU(pdu string) (out decodedPDU) {
	out.total, out.sequence = 1, 1
	raw, err := hex.DecodeString(pdu)
	if err != nil || len(raw) == 0 || 1+int(raw[0]) >= len(raw) {
		return
	}
	data := raw[1+int(raw[0]):]
	if len(data) < 13 || data[0]&3 != 0 {
		return
	}
	header := data[0]
	addressLength := int(data[1])
	addressBytes := (addressLength + 1) / 2
	if 13+addressBytes > len(data) {
		return
	}
	out.sender = decodeAddress(data[2], data[3:3+addressBytes], addressLength)
	data = data[3+addressBytes:]
	out.dcs = data[1]
	out.timestamp = decodeTime(data[2:9])
	length, payload := int(data[9]), data[10:]
	encoding := smsEncoding(out.dcs)
	if encoding < 0 {
		return
	}
	bytes := length
	if encoding == 0 {
		bytes = (length*7 + 7) / 8
	}
	if bytes != len(payload) {
		return
	}
	offset := 0
	if header&0x40 != 0 {
		if len(payload) == 0 || int(payload[0])+1 > len(payload) {
			return
		}
		offset = int(payload[0]) + 1
		if !readUserHeader(payload[1:offset], &out) {
			return
		}
	}
	switch encoding {
	case 0:
		septets := (offset*8 + 6) / 7
		if septets > length {
			return
		}
		out.text = decodeSeptets(payload, septets*7, length-septets)
	case 2:
		payload = payload[offset:]
		if len(payload)%2 != 0 {
			return
		}
		words := make([]uint16, len(payload)/2)
		for i := range words {
			words[i] = uint16(payload[i*2])<<8 | uint16(payload[i*2+1])
		}
		out.text = string(utf16.Decode(words))
	default:
		return
	}
	out.decoded = out.sender != ""
	return
}

// smsEncoding returns the alphabet (7-bit, 8-bit, UCS-2) or -1 for a
// reserved/compressed encoding.
func smsEncoding(dcs byte) int {
	switch {
	case dcs&0xc0 == 0 || dcs&0xc0 == 0x40:
		if dcs&0x20 == 0 {
			return int(dcs>>2) & 3
		}
	case dcs&0xf0 == 0xc0 || dcs&0xf0 == 0xd0:
		return 0
	case dcs&0xf0 == 0xe0:
		return 2
	case dcs&0xf0 == 0xf0:
		return int(dcs>>2) & 1
	}
	return -1
}

func readUserHeader(header []byte, out *decodedPDU) bool {
	concat := false
	for len(header) > 0 {
		if len(header) < 2 || 2+int(header[1]) > len(header) {
			return false
		}
		tag, size := header[0], int(header[1])
		value := header[2 : 2+size]
		header = header[2+size:]
		switch tag {
		case 0, 8:
			if concat || (tag == 0 && size != 3) || (tag == 8 && size != 4) {
				return false
			}
			concat = true
			out.ref = int(value[0])
			if tag == 8 {
				out.ref = 0x10000 + int(value[0])*256 + int(value[1])
			}
			out.total, out.sequence = int(value[size-2]), int(value[size-1])
			if out.total == 0 || out.sequence == 0 || out.sequence > out.total {
				out.total, out.sequence = 1, 1
				return false
			}
		case 0x24, 0x25:
			// Preserve the PDU of national language shift tables rather than
			// decode it incorrectly with the default alphabet.
			if size != 1 || value[0] != 0 {
				return false
			}
		}
	}
	return true
}

func decodeAddress(typ byte, data []byte, length int) string {
	if typ&0x70 == 0x50 {
		return decodeSeptets(data, 0, length*4/7)
	}
	const digits = "0123456789*#abc"
	var value strings.Builder
	if typ&0x70 == 0x10 {
		value.WriteByte('+')
	}
	for i := 0; i < length; i++ {
		digit := data[i/2] >> (4 * (i % 2)) & 15
		if int(digit) >= len(digits) {
			return ""
		}
		value.WriteByte(digits[digit])
	}
	return value.String()
}

var gsmAlphabet = []rune("@£$¥èéùìòÇ\nØø\rÅåΔ_ΦΓΛΩΠΨΣΘΞ\x1bÆæßÉ" +
	" !\"#¤%&'()*+,-./0123456789:;<=>?¡ABCDEFGHIJKLMNOPQRSTUVWXYZÄÖÑÜ§¿abcdefghijklmnopqrstuvwxyzäöñüà")

var gsmExtension = map[byte]rune{
	0x0a: '\f', 0x14: '^', 0x28: '{', 0x29: '}', 0x2f: '\\',
	0x3c: '[', 0x3d: '~', 0x3e: ']', 0x40: '|', 0x65: '€',
}

func decodeSeptets(data []byte, start, count int) string {
	var result strings.Builder
	escape := false
	for i := 0; i < count; i++ {
		position := start + i*7
		index, shift := position/8, position%8
		code := data[index] >> shift
		if shift > 1 && index+1 < len(data) {
			code |= data[index+1] << (8 - shift)
		}
		code &= 127
		switch {
		case escape:
			letter, ok := gsmExtension[code]
			if !ok {
				letter = ' '
			}
			result.WriteRune(letter)
			escape = false
		case code == 27:
			escape = true
		default:
			result.WriteRune(gsmAlphabet[code])
		}
	}
	return result.String()
}

func decodeTime(data []byte) *time.Time {
	negative := data[6]&8 != 0
	var parts [7]int
	for i, b := range data {
		if i == 6 {
			b &^= 8
		}
		tens, ones := b&15, b>>4
		if tens > 9 || ones > 9 {
			return nil
		}
		parts[i] = int(tens)*10 + int(ones)
	}
	year := parts[0] + 2000
	if parts[0] >= 90 {
		year -= 100
	}
	zone := parts[6] * 15 * 60
	if negative {
		zone = -zone
	}
	stamp := time.Date(year, time.Month(parts[1]), parts[2], parts[3], parts[4], parts[5], 0, time.FixedZone("", zone))
	if int(stamp.Month()) != parts[1] || stamp.Day() != parts[2] ||
		stamp.Hour() != parts[3] || stamp.Minute() != parts[4] || stamp.Second() != parts[5] {
		return nil
	}
	utc := stamp.UTC()
	return &utc
}
