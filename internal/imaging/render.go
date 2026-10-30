package imaging

import (
	"encoding/binary"
	"image"
	"math"
)

// Shrink scales an image down so neither side is over maxDim, averaging
// the pixels it merges. Viewing a 3,000-pixel X-ray on a screen doesn't
// need all of it.
func (p *Pixels) Shrink(maxDim int) *Pixels {
	if maxDim <= 0 || (p.Rows <= maxDim && p.Cols <= maxDim) {
		return p
	}
	f := int(math.Ceil(float64(max(p.Rows, p.Cols)) / float64(maxDim)))
	out := *p
	out.Rows, out.Cols = p.Rows/f, p.Cols/f
	if p.Values != nil {
		out.Values = make([]float32, out.Rows*out.Cols)
		for y := 0; y < out.Rows; y++ {
			for x := 0; x < out.Cols; x++ {
				var sum float32
				for dy := 0; dy < f; dy++ {
					row := (y*f + dy) * p.Cols
					for dx := 0; dx < f; dx++ {
						sum += p.Values[row+x*f+dx]
					}
				}
				out.Values[y*out.Cols+x] = sum / float32(f*f)
			}
		}
	} else {
		out.RGB = make([]byte, out.Rows*out.Cols*3)
		for y := 0; y < out.Rows; y++ {
			for x := 0; x < out.Cols; x++ {
				for c := 0; c < 3; c++ {
					sum := 0
					for dy := 0; dy < f; dy++ {
						for dx := 0; dx < f; dx++ {
							sum += int(p.RGB[((y*f+dy)*p.Cols+x*f+dx)*3+c])
						}
					}
					out.RGB[(y*out.Cols+x)*3+c] = byte(sum / (f * f))
				}
			}
		}
	}
	return &out
}

// Render draws the image for a person to look at: grayscale through the
// window (center and width; zero means the image's own), or color as is.
func (p *Pixels) Render(center, width float64) image.Image {
	if p.RGB != nil {
		img := image.NewRGBA(image.Rect(0, 0, p.Cols, p.Rows))
		for i := 0; i < p.Rows*p.Cols; i++ {
			img.Pix[i*4], img.Pix[i*4+1], img.Pix[i*4+2], img.Pix[i*4+3] = p.RGB[i*3], p.RGB[i*3+1], p.RGB[i*3+2], 255
		}
		return img
	}
	if width <= 0 {
		center, width = p.Center, p.Width
	}
	lo := center - width/2
	img := image.NewGray(image.Rect(0, 0, p.Cols, p.Rows))
	for i, v := range p.Values {
		g := (float64(v) - lo) / width * 255
		g = math.Max(0, math.Min(255, g))
		if p.Invert {
			g = 255 - g
		}
		img.Pix[i] = uint8(g)
	}
	return img
}

// Packed is a grayscale frame for the browser: 16-bit samples with the
// scale to turn them back into values, so window and level change in the
// browser without asking the server again.
type Packed struct {
	Rows   int     `json:"rows"`
	Cols   int     `json:"cols"`
	Offset float32 `json:"offset"` // value = sample/Scale + Offset
	Scale  float32 `json:"scale"`
	Center float64 `json:"center"`
	Width  float64 `json:"width"`
	Invert bool    `json:"invert"`
	Min    float32 `json:"min"`
	Max    float32 `json:"max"`
}

// Pack turns grayscale values into 16-bit samples, exactly when the range
// fits (CT, MRI and most X-rays do) and evenly spread when it doesn't.
func (p *Pixels) Pack() (Packed, []byte) {
	span := float64(p.Max - p.Min)
	scale := float32(1)
	if span > 65535 {
		scale = float32(65535 / span)
	}
	meta := Packed{Rows: p.Rows, Cols: p.Cols, Offset: p.Min, Scale: scale, Center: p.Center, Width: p.Width,
		Invert: p.Invert, Min: p.Min, Max: p.Max}
	out := make([]byte, len(p.Values)*2)
	for i, v := range p.Values {
		s := math.Round(float64((v - p.Min) * scale))
		binary.LittleEndian.PutUint16(out[i*2:], uint16(math.Max(0, math.Min(65535, s))))
	}
	return meta, out
}
