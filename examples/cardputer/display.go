//go:build tinygo

package main

import (
	"image/color"
	"machine"

	"tinygo.org/x/drivers/pixel"
	"tinygo.org/x/drivers/st7789"
	"tinygo.org/x/tinyfont"
	"tinygo.org/x/tinyfont/proggy"
)

// The LCD of the Cardputer: an ST7789V2 with 135×240 pixels, used in landscape.
const (
	screenWidth  = 240
	screenHeight = 135

	charWidth  = 6
	lineHeight = 11
	baseline   = 8 // of proggy.TinySZ8pt7b within a line

	columns = screenWidth / charWidth
	rows    = screenHeight / lineHeight
)

var (
	white  = color.RGBA{0xFF, 0xFF, 0xFF, 0xFF}
	grey   = color.RGBA{0x90, 0x90, 0x90, 0xFF}
	green  = color.RGBA{0x40, 0xFF, 0x60, 0xFF}
	yellow = color.RGBA{0xFF, 0xD0, 0x40, 0xFF}
	red    = color.RGBA{0xFF, 0x50, 0x50, 0xFF}
	cyan   = color.RGBA{0x40, 0xD0, 0xFF, 0xFF}
	black  = color.RGBA{0, 0, 0, 0xFF}
)

func openDisplay() *st7789.Device {
	machine.SPI0.Configure(machine.SPIConfig{
		Frequency: 40_000_000,
		SCK:       machine.GPIO36,
		SDO:       machine.GPIO35,
		SDI:       machine.NoPin,
	})

	d := st7789.New(machine.SPI0, machine.GPIO33, machine.GPIO34, machine.GPIO37, machine.GPIO38)
	d.Configure(st7789.Config{
		Width:        135,
		Height:       240,
		Rotation:     st7789.ROTATION_90,
		RowOffset:    40,
		ColumnOffset: 52,
	})
	d.EnableBacklight(true)

	return &d
}

// line is one text line of the screen, drawn in memory and sent in one go: drawing a font
// pixel by pixel over SPI would be far too slow.
type line struct {
	img pixel.Image[pixel.RGB565BE]
	bg  color.RGBA
}

func (l *line) Size() (int16, int16) { return screenWidth, lineHeight }

func (l *line) SetPixel(x, y int16, c color.RGBA) {
	if x >= 0 && x < screenWidth && y >= 0 && y < lineHeight {
		l.img.Set(int(x), int(y), pixel.NewColor[pixel.RGB565BE](c.R, c.G, c.B))
	}
}

func (l *line) Display() error { return nil }

func (l *line) clear(bg color.RGBA) {
	l.bg = bg
	l.img.FillSolidColor(pixel.NewColor[pixel.RGB565BE](bg.R, bg.G, bg.B))
}

// console is a scrolling text console: the top row is a status line, the bottom row the input.
type console struct {
	display *st7789.Device
	buf     line

	lines  [rows - 2]string
	colors [rows - 2]color.RGBA
}

func newConsole(d *st7789.Device) *console {
	c := &console{display: d, buf: line{img: pixel.NewImage[pixel.RGB565BE](screenWidth, lineHeight)}}
	d.FillScreen(black)

	return c
}

// drawRow draws text on a row of the screen.
func (c *console) drawRow(row int, text string, fg, bg color.RGBA) {
	if len(text) > columns {
		text = text[:columns]
	}

	c.buf.clear(bg)
	tinyfont.WriteLine(&c.buf, &proggy.TinySZ8pt7b, 0, baseline, text, fg)
	c.display.DrawBitmap(0, int16(row*lineHeight), c.buf.img)
}

func (c *console) status(text string, fg color.RGBA) {
	c.drawRow(0, text, fg, color.RGBA{0x20, 0x20, 0x40, 0xFF})
}

func (c *console) input(text string) {
	if len(text) > columns-3 {
		text = text[len(text)-(columns-3):]
	}

	c.drawRow(rows-1, "> "+text+"_", white, color.RGBA{0x10, 0x10, 0x10, 0xFF})
}

// print adds a line at the bottom of the scrolling area, wrapped at the screen width.
func (c *console) print(text string, fg color.RGBA) {
	println(text)

	for {
		part := text
		if len(part) > columns {
			part = text[:columns]
		}

		copy(c.lines[:], c.lines[1:])
		copy(c.colors[:], c.colors[1:])
		c.lines[len(c.lines)-1] = part
		c.colors[len(c.colors)-1] = fg

		if len(text) <= columns {
			break
		}

		text = text[columns:]
	}

	for i, s := range c.lines {
		c.drawRow(1+i, s, c.colors[i], black)
	}
}
