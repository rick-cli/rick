package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// The startup mascot.
//
// The source is a colour-per-pixel grid, which a terminal cannot show
// directly: a text cell is roughly twice as tall as it is wide. Rendering one
// pixel per cell would produce a portrait stretched to twice its height.
//
// Instead each cell carries TWO vertical pixels using the half-block glyph
// "▀": the foreground colour paints the upper pixel, the background the
// lower. That restores the aspect ratio and doubles the effective resolution.

const artAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz#$"

// artRGB is artPalette pre-parsed, so sampling never re-parses hex strings.
var artRGB = func() [][3]uint8 {
	out := make([][3]uint8, len(artPalette))
	for i, h := range artPalette {
		var r, g, b int
		fmt.Sscanf(h, "#%02x%02x%02x", &r, &g, &b)
		out[i] = [3]uint8{uint8(r), uint8(g), uint8(b)}
	}
	return out
}()

// nearestArt finds the palette entry closest to a colour. The palette is
// small and results are cached by renderArt, so a linear scan is fine.
func nearestArt(r, g, b uint8) uint8 {
	best, bestD := uint8(1), 1<<30
	for i := 1; i < len(artRGB); i++ {
		c := artRGB[i]
		dr, dg, db := int(c[0])-int(r), int(c[1])-int(g), int(c[2])-int(b)
		if d := dr*dr + dg*dg + db*db; d < bestD {
			best, bestD = uint8(i), d
		}
	}
	return best
}

// artImage is the decoded pixel grid, indices into artPalette.
type artImage struct {
	w, h int
	px   []uint8
}

var decodedArt *artImage

// loadArt decodes the run-length payload once.
func loadArt() *artImage {
	if decodedArt != nil {
		return decodedArt
	}
	idx := map[byte]int{}
	for i := 0; i < len(artAlphabet); i++ {
		idx[artAlphabet[i]] = i
	}

	px := make([]uint8, 0, artWidth*artHeight)
	for i := 0; i+1 < len(artPixels); i += 2 {
		v := uint8(idx[artPixels[i]])
		n := idx[artPixels[i+1]]
		for j := 0; j < n; j++ {
			px = append(px, v)
		}
	}
	for len(px) < artWidth*artHeight {
		px = append(px, 0)
	}
	decodedArt = &artImage{w: artWidth, h: artHeight, px: px[:artWidth*artHeight]}
	return decodedArt
}

// at returns the palette index at a pixel, 0 (transparent) when out of range.
func (a *artImage) at(x, y int) uint8 {
	if x < 0 || y < 0 || x >= a.w || y >= a.h {
		return 0
	}
	return a.px[y*a.w+x]
}

// sample averages a source rectangle and returns the closest palette entry.
//
// Picking the single most common colour instead — the obvious shortcut —
// discards every gradient: one cell spans several source pixels, so a winner-
// takes-all choice turns smooth shading into hard bands and the face reads as
// blocky mush. Averaging is what a real downscale does.
//
// Transparent pixels are excluded from the average but counted: a cell that is
// mostly background stays background, so the silhouette keeps its edge.
func (a *artImage) sample(x0, y0, x1, y1 int) uint8 {
	var r, g, b, n, total int
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			total++
			v := a.at(x, y)
			if v == 0 {
				continue
			}
			c := artRGB[v]
			r += int(c[0])
			g += int(c[1])
			b += int(c[2])
			n++
		}
	}
	if n == 0 || n*2 < total {
		return 0 // majority background: keep the cell empty
	}
	return nearestArt(uint8(r/n), uint8(g/n), uint8(b/n))
}

// artCache memoises rendered output. The mascot carries its own palette and
// is independent of the theme, so width is the only variable and a cached
// line can never go stale. Re-styling ~600 cells every frame cost half a
// millisecond for a byte-identical result.
var artCache = map[int][]string{}

// artCacheOrder is the FIFO of cached widths; a pathological resize loop must
// not grow the cache without limit.
var artCacheOrder []int

const artCacheMaxWidths = 64

func cacheArt(cols int, lines []string) {
	if len(artCacheOrder) >= artCacheMaxWidths {
		oldest := artCacheOrder[0]
		artCacheOrder = artCacheOrder[1:]
		delete(artCache, oldest)
	}
	artCacheOrder = append(artCacheOrder, cols)
	artCache[cols] = lines
}

// renderArt draws the mascot at the given cell width. Height is derived from
// the source aspect ratio; half-blocks mean one cell spans two pixels.
func renderArt(cols int) []string {
	if cols < 8 {
		return nil
	}
	if got, ok := artCache[cols]; ok {
		return got
	}
	a := loadArt()

	// One cell holds two stacked pixels, so rows = (h/sx)/2. This assumes the
	// source has SQUARE pixels — which is why the art is now generated from
	// the PNG. The earlier HTML export's "pixels" were character cells, each
	// already twice as tall as it was wide, so its 150x187 really described a
	// 150x374 image and everything rendered at half its proper height.
	sx := float64(a.w) / float64(cols)
	rows := int(float64(a.h)/sx/2 + 0.5)
	if rows < 1 {
		rows = 1
	}
	sy := float64(a.h) / float64(rows*2)

	lines := make([]string, 0, rows)
	for r := 0; r < rows; r++ {
		var b strings.Builder
		for c := 0; c < cols; c++ {
			x0 := int(float64(c) * sx)
			x1 := int(float64(c+1) * sx)
			if x1 <= x0 {
				x1 = x0 + 1
			}
			yTop := int(float64(r*2) * sy)
			yMid := int(float64(r*2+1) * sy)
			yBot := int(float64(r*2+2) * sy)
			if yMid <= yTop {
				yMid = yTop + 1
			}
			if yBot <= yMid {
				yBot = yMid + 1
			}

			up := a.sample(x0, yTop, x1, yMid)
			lo := a.sample(x0, yMid, x1, yBot)
			b.WriteString(halfBlock(up, lo))
		}
		lines = append(lines, b.String())
	}
	cacheArt(cols, lines)
	return lines
}

// halfBlock renders one cell holding two stacked pixels.
func halfBlock(up, lo uint8) string {
	switch {
	case up == 0 && lo == 0:
		return " "
	case up != 0 && lo == 0:
		return lipgloss.NewStyle().Foreground(lipgloss.Color(artPalette[up])).Render("▀")
	case up == 0 && lo != 0:
		return lipgloss.NewStyle().Foreground(lipgloss.Color(artPalette[lo])).Render("▄")
	default:
		return lipgloss.NewStyle().
			Foreground(lipgloss.Color(artPalette[up])).
			Background(lipgloss.Color(artPalette[lo])).
			Render("▀")
	}
}

// ArtDimensions is the stored pixel size of the mascot (test helper).
func ArtDimensions() (int, int) { return artWidth, artHeight }

// RenderArt draws the mascot at a given width (test helper).
func RenderArt(cols int) []string { return renderArt(cols) }

// ArtHeightFor reports the row count for a width (test helper).
func ArtHeightFor(cols int) int { return artHeightFor(cols) }

// artHeightFor reports how many rows renderArt(cols) will produce.
func artHeightFor(cols int) int {
	if cols < 8 {
		return 0
	}
	a := loadArt()
	sx := float64(a.w) / float64(cols)
	rows := int(float64(a.h)/sx/2 + 0.5)
	if rows < 1 {
		rows = 1
	}
	return rows
}
