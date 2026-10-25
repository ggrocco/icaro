//go:build ignore

// Generates the app's icons: go generate ./icons (from desktop/).
// The mark is the web UI's ☀︎: a disc with eight rays.
package main

import (
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"os"
)

func main() {
	write("tray-template.png", sun(44, color.NRGBA{A: 255}, color.NRGBA{}, false))
	write("tray.png", sun(64, color.NRGBA{R: 0xe8, G: 0x8a, B: 0x1a, A: 255}, color.NRGBA{}, false))
	write("app.png", sun(1024, color.NRGBA{R: 0xff, G: 0xb3, B: 0x2e, A: 255}, color.NRGBA{R: 0x1d, G: 0x23, B: 0x33, A: 255}, true))
}

// sun draws the mark with 4x4 supersampling for smooth edges. With a
// background, it sits on a rounded square (macOS app icon grid).
func sun(size int, fg, bg color.NRGBA, tile bool) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	s := float64(size)
	const ss = 4
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			var fgHits, bgHits int
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					px := (float64(x) + (float64(sx)+0.5)/ss) / s
					py := (float64(y) + (float64(sy)+0.5)/ss) / s
					switch {
					case inSun(px, py, tile):
						fgHits++
					case tile && inTile(px, py):
						bgHits++
					}
				}
			}
			img.SetNRGBA(x, y, blend(fg, bg, fgHits, bgHits, ss*ss))
		}
	}
	return img
}

func inSun(x, y float64, tile bool) bool {
	scale := 1.0
	if tile {
		scale = 0.62 // leave the tile's margin
	}
	dx, dy := (x-0.5)/scale, (y-0.5)/scale
	r := math.Hypot(dx, dy)
	if r <= 0.21 {
		return true
	}
	// Rays: eight short bars between r=0.30 and r=0.47.
	if r < 0.30 || r > 0.47 {
		return false
	}
	a := math.Atan2(dy, dx)
	step := math.Pi / 4
	nearest := math.Round(a/step) * step
	return math.Abs(math.Sin(a-nearest))*r <= 0.045
}

func inTile(x, y float64) bool {
	const m, rad = 0.1, 0.18 // margin and corner radius
	if x < m || x > 1-m || y < m || y > 1-m {
		return false
	}
	cx := math.Max(m+rad-x, math.Max(0, x-(1-m-rad)))
	cy := math.Max(m+rad-y, math.Max(0, y-(1-m-rad)))
	return math.Hypot(cx, cy) <= rad
}

func blend(fg, bg color.NRGBA, f, b, n int) color.NRGBA {
	if f+b == 0 {
		return color.NRGBA{}
	}
	mix := func(a, c uint8) uint8 { return uint8((int(a)*f + int(c)*b) / (f + b)) }
	alpha := (int(fg.A)*f + int(bg.A)*b) / n
	return color.NRGBA{R: mix(fg.R, bg.R), G: mix(fg.G, bg.G), B: mix(fg.B, bg.B), A: uint8(alpha)}
}

func write(name string, img image.Image) {
	f, err := os.Create(name)
	if err != nil {
		log.Fatal(err)
	}
	if err := png.Encode(f, img); err != nil {
		log.Fatal(err)
	}
	if err := f.Close(); err != nil {
		log.Fatal(err)
	}
}
