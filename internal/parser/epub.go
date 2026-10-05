package parser

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"strings"
	"time"

	"github.com/disintegration/imaging"
)

type Metadata struct {
	Title         string
	Creators      []string
	Language      string
	Description   string
	Publisher     string
	PublishedDate time.Time
}

type ParseResult struct {
	Meta  Metadata
	Cover []byte // JPEG bytes, empty if none found
}

const maxCoverWidth = 600

// Parse reads metadata and cover from an EPUB (zip) archive. It only opens
// the small XML entries plus the cover image; book blobs are never read.
func Parse(r io.ReaderAt, size int64) (*ParseResult, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, fmt.Errorf("open zip: %w", err)
	}
	opfPath, err := readContainer(zr, "META-INF/container.xml")
	if err != nil {
		return nil, err
	}
	opfData, err := readEntry(zr, opfPath, 4<<20)
	if err != nil {
		return nil, fmt.Errorf("read opf: %w", err)
	}
	meta, coverRef, err := parseOPF(opfData)
	if err != nil {
		return nil, err
	}
	if coverRef != "" {
		coverRef = resolveRef(opfPath, coverRef)
	}
	if coverRef == "" {
		coverRef = firstImage(zr)
	}
	res := &ParseResult{Meta: meta}
	if coverRef != "" {
		if raw, err := readEntry(zr, coverRef, 16<<20); err == nil && len(raw) > 0 {
			res.Cover = resizeCover(raw)
		}
	}
	if res.Meta.Title == "" {
		res.Meta.Title = "Unknown"
	}
	return res, nil
}

func readEntry(zr *zip.Reader, name string, limit int64) ([]byte, error) {
	f := findFile(zr, name)
	if f == nil {
		return nil, fmt.Errorf("entry %q not found", name)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, limit))
}

func readContainer(zr *zip.Reader, name string) (string, error) {
	data, err := readEntry(zr, name, 1<<20)
	if err != nil {
		return "", err
	}
	var ct struct {
		Rootfiles []struct {
			FullPath string `xml:"full-path,attr"`
		} `xml:"rootfiles>rootfile"`
	}
	if err := xml.Unmarshal(data, &ct); err != nil {
		return "", err
	}
	if len(ct.Rootfiles) == 0 {
		return "", fmt.Errorf("no rootfile in container.xml")
	}
	return ct.Rootfiles[0].FullPath, nil
}

func parseOPF(data []byte) (Metadata, string, error) {
	var m Metadata
	var coverID string
	var itemsByID map[string]string
	var coverPropHref string

	dec := xml.NewDecoder(bytes.NewReader(data))
	var inMetadata, inManifest bool
	var elemText strings.Builder
	var collecting string // element local name currently collecting char data

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return m, "", fmt.Errorf("parse opf: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			name := t.Name.Local
			switch name {
			case "metadata":
				inMetadata = true
			case "manifest":
				inManifest = true
			case "meta":
				if inMetadata {
					var nameAttr, content string
					for _, a := range t.Attr {
						switch a.Name.Local {
						case "name":
							nameAttr = a.Value
						case "content":
							content = a.Value
						}
					}
					setMeta(&m, nameAttr, content, &coverID)
				}
			case "item":
				if inManifest {
					if itemsByID == nil {
						itemsByID = map[string]string{}
					}
					id, href, props := "", "", ""
					for _, a := range t.Attr {
						switch a.Name.Local {
						case "id":
							id = a.Value
						case "href":
							href = a.Value
						case "properties":
							props = a.Value
						}
					}
					if id != "" {
						itemsByID[id] = href
					}
					if strings.Contains(props, "cover-image") {
						coverPropHref = href
					}
				}
			case "title", "creator", "language", "description", "publisher", "date":
				if inMetadata {
					collecting = name
					elemText.Reset()
				}
			}
		case xml.CharData:
			if collecting != "" {
				elemText.Write(t)
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "metadata":
				inMetadata = false
			case "manifest":
				inManifest = false
			case "title", "creator", "language", "description", "publisher", "date":
				if collecting == t.Name.Local {
					setElem(&m, t.Name.Local, strings.TrimSpace(elemText.String()))
					collecting = ""
					elemText.Reset()
				}
			}
		}
	}

	coverRef := ""
	if coverPropHref != "" {
		coverRef = coverPropHref
	} else if id, ok := itemsByID[coverID]; ok {
		coverRef = id
	} else if coverID != "" {
		coverRef = coverID
	}
	return m, coverRef, nil
}

func setMeta(m *Metadata, name, content string, coverID *string) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "cover":
		*coverID = content
	case "dc:title", "http://purl.org/dc/elements/1.1/title":
		if m.Title == "" {
			m.Title = strings.TrimSpace(content)
		}
	case "dc:creator", "http://purl.org/dc/elements/1.1/creator":
		m.Creators = append(m.Creators, strings.TrimSpace(content))
	case "dc:language", "http://purl.org/dc/elements/1.1/language":
		m.Language = strings.TrimSpace(content)
	case "dc:description", "http://purl.org/dc/elements/1.1/description":
		m.Description = strings.TrimSpace(content)
	case "dc:publisher", "http://purl.org/dc/elements/1.1/publisher":
		m.Publisher = strings.TrimSpace(content)
	case "dc:date", "http://purl.org/dc/elements/1.1/date":
		if t, err := parseDCDate(content); err == nil {
			m.PublishedDate = t
		}
	}
}

func setElem(m *Metadata, name, content string) {
	switch name {
	case "title":
		if m.Title == "" {
			m.Title = content
		}
	case "creator":
		m.Creators = append(m.Creators, content)
	case "language":
		if m.Language == "" {
			m.Language = content
		}
	case "description":
		if m.Description == "" {
			m.Description = content
		}
	case "publisher":
		if m.Publisher == "" {
			m.Publisher = content
		}
	case "date":
		if m.PublishedDate.IsZero() {
			if t, err := parseDCDate(content); err == nil {
				m.PublishedDate = t
			}
		}
	}
}

// resolveRef resolves an OPF item href (relative to the OPF file's dir) to
// an archive entry path.
func resolveRef(opfPath, ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	if strings.HasPrefix(ref, "/") {
		return strings.TrimPrefix(ref, "/")
	}
	dir := strings.LastIndex(opfPath, "/")
	if dir < 0 {
		return ref
	}
	return strings.Join([]string{opfPath[:dir], ref}, "/")
}

func firstImage(zr *zip.Reader) string {
	for _, f := range zr.File {
		lower := strings.ToLower(f.Name)
		switch {
		case strings.HasSuffix(lower, ".png"), strings.HasSuffix(lower, ".jpg"),
			strings.HasSuffix(lower, ".jpeg"), strings.HasSuffix(lower, ".webp"),
			strings.HasSuffix(lower, ".gif"):
			return f.Name
		}
	}
	return ""
}

func resizeCover(raw []byte) []byte {
	dec, err := imaging.Decode(bytes.NewReader(raw))
	if err != nil {
		return raw
	}
	if dec.Bounds().Dx() > maxCoverWidth {
		dec = imaging.Resize(dec, maxCoverWidth, 0, imaging.Lanczos)
	}
	var buf bytes.Buffer
	if err := imaging.Encode(&buf, dec, imaging.JPEG); err != nil {
		return raw
	}
	return buf.Bytes()
}

func parseDCDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{"2006-01-02", "2006", time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized date %q", s)
}

func findFile(zr *zip.Reader, name string) *zip.File {
	normalized := normalizeName(name)
	for _, f := range zr.File {
		if normalizeName(f.Name) == normalized {
			return f
		}
	}
	return nil
}

func normalizeName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.TrimPrefix(name, "/")
	for strings.Contains(name, "/./") {
		name = strings.ReplaceAll(name, "/./", "/")
	}
	return name
}
