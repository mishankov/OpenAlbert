package desktopapi

import (
	"encoding/binary"
	"errors"
	"mime"
	"net/http"
)

// attachmentMIME handles the M4A container that Go's HTTP sniffer labels video/mp4.
// Neither a filename nor a claimed audio MIME is sufficient to reinterpret video
// as a voice message. The STT decoder remains responsible for codec, channels and
// real duration validation; this bounded parser only establishes the media route.
func attachmentMIME(data []byte, claimed string) (string, error) {
	kind := http.DetectContentType(data)
	declared, _, _ := mime.ParseMediaType(claimed)
	if audioMP4(data) {
		return "audio/mp4", nil
	}
	if declared == "audio/mp4" {
		return "", errors.New("Expected a valid audio-only M4A attachment")
	}
	if declared != "" && kind == "application/octet-stream" {
		kind = declared
	}
	return kind, nil
}

// mp4Boxes visits sibling boxes. All offsets are bounded by the supplied slice;
// extended sizes, boxes consuming their parent and malformed lengths are handled
// without trusting an attacker-controlled allocation or recursion depth.
func mp4Boxes(data []byte, visit func(string, []byte) bool) bool {
	for len(data) > 0 {
		if len(data) < 8 {
			return false
		}
		size := uint64(binary.BigEndian.Uint32(data[:4]))
		header := uint64(8)
		if size == 1 {
			if len(data) < 16 {
				return false
			}
			size = binary.BigEndian.Uint64(data[8:16])
			header = 16
		} else if size == 0 {
			size = uint64(len(data))
		}
		if size < header || size > uint64(len(data)) {
			return false
		}
		if !visit(string(data[4:8]), data[int(header):int(size)]) {
			return false
		}
		data = data[int(size):]
	}
	return true
}

func audioMP4(data []byte) bool {
	branded, media, audio, video := false, false, false, false
	valid := mp4Boxes(data, func(kind string, body []byte) bool {
		switch kind {
		case "ftyp":
			if len(body) < 8 || len(body)%4 != 0 {
				return false
			}
			for offset := 0; offset < len(body); offset += 4 {
				if offset == 4 {
					continue
				}
				switch string(body[offset : offset+4]) {
				case "M4A ", "M4B ", "isom", "iso2", "mp41", "mp42":
					branded = true
				}
			}
		case "mdat":
			media = media || len(body) > 0
		case "moov":
			return mp4Boxes(body, func(kind string, track []byte) bool {
				if kind != "trak" {
					return true
				}
				return mp4Boxes(track, func(kind string, mdia []byte) bool {
					if kind != "mdia" {
						return true
					}
					return mp4Boxes(mdia, func(kind string, handler []byte) bool {
						if kind != "hdlr" {
							return true
						}
						if len(handler) < 12 {
							return false
						}
						switch string(handler[8:12]) {
						case "soun":
							audio = true
						case "vide":
							video = true
						}
						return true
					})
				})
			})
		}
		return true
	})
	return valid && branded && media && audio && !video
}
