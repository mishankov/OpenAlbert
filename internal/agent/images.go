package agent

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
)

// smallImage keeps image messages below Temporal's payload limit and bounds decoded memory.
func smallImage(data []byte) ([]byte, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if int64(cfg.Width)*int64(cfg.Height) > 40_000_000 {
		return nil, errors.New("image resolution exceeds limit")
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	w, h := cfg.Width, cfg.Height
	if w > 512 || h > 512 {
		if w >= h {
			h = h * 512 / w
			w = 512
		} else {
			w = w * 512 / h
			h = 512
		}
	}
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	bounds := src.Bounds()
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := src.At(bounds.Min.X+x*cfg.Width/w, bounds.Min.Y+y*cfg.Height/h)
			r, g, b, alpha := c.RGBA()
			if alpha < 65535 {
				r += 65535 - alpha
				g += 65535 - alpha
				b += 65535 - alpha
			}
			dst.SetRGBA(x, y, color.RGBA{uint8(r >> 8), uint8(g >> 8), uint8(b >> 8), 255})
		}
	}
	var out bytes.Buffer
	err = jpeg.Encode(&out, dst, &jpeg.Options{Quality: 60})
	return out.Bytes(), err
}
