package integrations

import "encoding/binary"

// validOggOpus checks the framing of one complete logical stream without decoding audio.
func validOggOpus(data []byte) bool {
	const continuedFlag, startFlag, endFlag = 1, 2, 4
	var serial uint32
	packet := 0
	continued := false
	for page := uint32(0); len(data) > 0; page++ {
		if len(data) < 27 || string(data[:4]) != "OggS" || data[4] != 0 {
			return false
		}
		flags := data[5]
		if flags&^7 != 0 || (flags&startFlag != 0) != (page == 0) || (flags&continuedFlag != 0) != continued {
			return false
		}
		stream := binary.LittleEndian.Uint32(data[14:18])
		if page == 0 {
			serial = stream
		}
		if stream != serial || binary.LittleEndian.Uint32(data[18:22]) != page {
			return false
		}
		// One byte per segment gives its length. A length of 255 means the
		// packet continues into another segment, possibly on the next page.
		headerSize := 27 + int(data[26])
		if headerSize == 27 || len(data) < headerSize {
			return false
		}
		segments := data[27:headerSize]
		pageSize := headerSize
		for _, length := range segments {
			pageSize += int(length)
		}
		if len(data) < pageSize || oggChecksum(data[:pageSize]) != binary.LittleEndian.Uint32(data[22:26]) {
			return false
		}
		payload := data[headerSize:pageSize]
		for _, length := range segments {
			if !continued && !validOpusPacketStart(packet, payload[:length]) {
				return false
			}
			payload = payload[length:]
			continued = length == 255
			if !continued {
				packet++
			}
		}
		// RFC 7845 places the identification header alone on the first page.
		if page == 0 && (packet != 1 || continued) {
			return false
		}
		data = data[pageSize:]
		if flags&endFlag != 0 {
			return len(data) == 0 && !continued && packet >= 3
		}
	}
	return false
}

func validOpusPacketStart(packet int, data []byte) bool {
	switch packet {
	case 0:
		return len(data) >= 19 && string(data[:8]) == "OpusHead" && data[8] < 16 && data[9] > 0
	case 1:
		return len(data) >= 16 && string(data[:8]) == "OpusTags"
	default:
		return len(data) > 0
	}
}

// Ogg uses a non-reflected CRC-32 and calculates it with the checksum field zeroed.
var oggCRCTable = func() [256]uint32 {
	var table [256]uint32
	for i := range table {
		crc := uint32(i) << 24
		for range 8 {
			if crc&(1<<31) != 0 {
				crc = crc<<1 ^ 0x04C11DB7
			} else {
				crc <<= 1
			}
		}
		table[i] = crc
	}
	return table
}()

func oggChecksum(page []byte) uint32 {
	var crc uint32
	for i, value := range page {
		if i >= 22 && i < 26 {
			value = 0
		}
		crc = crc<<8 ^ oggCRCTable[byte(crc>>24)^value]
	}
	return crc
}
