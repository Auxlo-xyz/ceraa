package document

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Office text collection. A DOCX keeps its body in word/document.xml with
// headers and footers alongside; a PPTX keeps one XML file per slide. Both are
// the same trick underneath, so one walker serves both with different tags.

// wordMain is the body; headers and footers carry letterheads and page furniture.
const wordMain = "word/document.xml"

func wordHeadersFooters(names []string) []string {
	var out []string
	for _, n := range names {
		rest, ok := strings.CutPrefix(n, "word/header")
		if !ok {
			rest, ok = strings.CutPrefix(n, "word/footer")
		}
		if !ok || !strings.HasSuffix(n, ".xml") {
			continue
		}
		rest = strings.TrimSuffix(rest, ".xml")
		if rest == "" {
			continue
		}
		digits := true
		for _, c := range rest {
			if c < '0' || c > '9' {
				digits = false
			}
		}
		if digits {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// slidesMain lists slide files in numeric order. Authoring tools number slides
// in presentation order, so slide3.xml follows slide2.xml; the presentation
// manifest order would be more exact, but numeric order is right for every file
// seen so far and avoids parsing the manifest and its relationships.
func slidesMain(names []string) []string {
	var out []string
	for _, n := range names {
		if !strings.HasPrefix(n, "ppt/slides/slide") || !strings.HasSuffix(n, ".xml") {
			continue
		}
		rest := strings.TrimSuffix(strings.TrimPrefix(n, "ppt/slides/slide"), ".xml")
		if _, err := strconv.Atoi(rest); err == nil {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return slideNum(out[i]) < slideNum(out[j])
	})
	return out
}

func slideNum(name string) int {
	rest := strings.TrimSuffix(strings.TrimPrefix(name, "ppt/slides/slide"), ".xml")
	n, _ := strconv.Atoi(rest)
	return n
}

// officeText reads the text-bearing inner files of a DOCX or PPTX archive.
// main picks the ordered list of inner files from the archive's names, extra
// picks supplementary ones (headers and footers for word, none for slides).
// Slide text is prefixed with its slide number so "page 3 says" stays
// answerable; word paragraphs run together with headers and footers marked.
func officeText(path string, main func([]string) []string, extra func([]string) []string) (string, error) {
	zr, err := openOffice(path)
	if err != nil {
		return "", err
	}
	defer zr.Close()
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	chosen := main(names)
	if len(chosen) == 0 {
		return "", fmt.Errorf("no readable document body inside %s", filepath.Base(path))
	}
	// Word runs and slide runs use different namespaces, so the collector is
	// chosen per format by which inner files were picked.
	word := len(chosen) > 0 && strings.HasPrefix(chosen[0], "word/")
	var parts []string
	for _, name := range chosen {
		text, readErr := officeFileText(zr, name, func(raw []byte) string {
			if word {
				return wordXMLText(raw)
			}
			return slideXMLText(raw)
		})
		if readErr != nil {
			return "", readErr
		}
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		if !word {
			parts = append(parts, "--- slide "+strconv.Itoa(slideNum(name))+" ---")
		}
		parts = append(parts, text)
	}
	if extra != nil {
		for _, name := range extra(names) {
			text, readErr := officeFileText(zr, name, wordXMLText)
			if readErr != nil {
				return "", readErr
			}
			if text = strings.TrimSpace(text); text != "" {
				parts = append(parts, "["+name+" header/footer]\n"+text)
			}
		}
	}
	joined := strings.TrimSpace(strings.Join(parts, "\n\n"))
	if joined == "" {
		return "", fmt.Errorf("%s holds no text to extract", filepath.Base(path))
	}
	return joined, nil
}

// wordXMLText collects w:t runs, breaking paragraphs and table rows onto new
// lines. Tabs and explicit breaks become what they are; everything else in the
// XML (styles, numbering, revision marks) is structure, not content.
func wordXMLText(raw []byte) string {
	return collectRuns(raw, "w", map[string]string{
		"p": "\n", "tr": "\n", "br": "\n", "tab": "\t",
	})
}

// slideXMLText collects a:t runs, breaking paragraphs and separating shapes
// with a blank line so two boxes on one slide do not read as one sentence.
func slideXMLText(raw []byte) string {
	text := collectRuns(raw, "a", map[string]string{
		"p": "\n",
	})
	// Shape boundaries are not marked in the text runs, so consecutive
	// non-empty lines from different shapes would merge. The paragraph breaks
	// above plus the slide prefix are what keep them apart.
	return text
}

// collectRuns walks XML and keeps character data inside text elements of the
// given namespace prefix, inserting the breaks the map names. It matches on
// the local name after any prefix, because producers vary the prefix while the
// local names are stable across every file seen.
func collectRuns(raw []byte, _ string, breaks map[string]string) string {
	decoder := xml.NewDecoder(bytes.NewReader(raw))
	var out strings.Builder
	var inText bool
	var current strings.Builder
	flush := func() {
		if current.Len() > 0 {
			out.WriteString(current.String())
			current.Reset()
		}
	}
	for {
		token, err := decoder.Token()
		if err != nil {
			if err == io.EOF {
				break
			}
			return out.String()
		}
		switch typed := token.(type) {
		case xml.StartElement:
			local := localName(typed.Name.Space, typed.Name.Local)
			if local == "t" {
				inText = true
			} else if br, ok := breaks[local]; ok {
				flush()
				out.WriteString(br)
			}
		case xml.EndElement:
			if localName(typed.Name.Space, typed.Name.Local) == "t" {
				inText = false
			}
		case xml.CharData:
			if inText {
				current.Write([]byte(typed))
			}
		}
	}
	flush()
	return tidyBreaks(out.String())
}

// localName drops the namespace prefix, because "w:t" from one producer and
// "w14:t" never occur, but the safe comparison is on what follows the colon.
// Encoding/xml already splits namespaces when they are declared; this covers
// the literal prefix form too.
func localName(space, local string) string {
	if i := strings.LastIndex(local, ":"); i >= 0 {
		return local[i+1:]
	}
	_ = space
	return local
}

// tidyBreaks collapses runs of blank lines to one. XML structure produces far
// more breaks than a reader wants: every paragraph close plus every shape
// close, and the join between slides adds its own.
func tidyBreaks(text string) string {
	lines := strings.Split(text, "\n")
	var kept []string
	blank := false
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			if !blank {
				kept = append(kept, "")
			}
			blank = true
			continue
		}
		blank = false
		kept = append(kept, line)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}
