package parser

import (
	"archive/zip"
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
	"time"
)

func testEPUB(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range map[string]string{
		"META-INF/container.xml": `<container xmlns="urn:oasis:names:tc:opendocument:xmlns:container"><rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles></container>`,
		"OEBPS/content.opf": `<package xmlns="http://www.idpf.org/2007/opf" version="3.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:title>Test Title</dc:title>
    <dc:creator>Test Author</dc:creator>
    <dc:language>en</dc:language>
    <dc:date>2020-01-02</dc:date>
    <meta name="cover" content="cover-image"/>
  </metadata>
  <manifest><item id="cover-image" href="cover.png" media-type="image/png" properties="cover-image"/></manifest>
  <spine><itemref idref="x"/></spine></package>`,
	} {
		w, _ := zw.Create(name)
		_, _ = w.Write([]byte(content))
	}
	cw, _ := zw.Create("OEBPS/cover.png")
	img := image.NewRGBA(image.Rect(0, 0, 1000, 500))
	for y := 0; y < 500; y++ {
		for x := 0; x < 1000; x++ {
			img.Set(x, y, color.RGBA{1, 2, 3, 255})
		}
	}
	if err := png.Encode(cw, img); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestParse(t *testing.T) {
	data := testEPUB(t)
	res, err := Parse(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if res.Meta.Title != "Test Title" {
		t.Errorf("title = %q", res.Meta.Title)
	}
	if len(res.Meta.Creators) != 1 || res.Meta.Creators[0] != "Test Author" {
		t.Errorf("creators = %v", res.Meta.Creators)
	}
	if res.Meta.Language != "en" {
		t.Errorf("language = %q", res.Meta.Language)
	}
	want := time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC)
	if !res.Meta.PublishedDate.Equal(want) {
		t.Errorf("date = %v, want %v", res.Meta.PublishedDate, want)
	}
	if len(res.Cover) == 0 {
		t.Fatal("no cover extracted")
	}
	dec, err := jpeg.Decode(bytes.NewReader(res.Cover))
	if err != nil {
		t.Fatalf("cover not JPEG: %v", err)
	}
	if dec.Bounds().Dx() > maxCoverWidth {
		t.Errorf("cover width %d > %d", dec.Bounds().Dx(), maxCoverWidth)
	}
}
