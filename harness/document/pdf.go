package document

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Minimal PDF text extraction, stdlib only.
//
// A PDF page draws text with operators in a content stream: (Hello) Tj shows a
// literal string, [(Hel) -250 (lo)] TJ shows an array with spacing. The stream
// is usually FlateDecode compressed, which is zlib and decodes with the
// standard library. What is NOT handled: other compressions, encrypted files,
// object streams, text drawn as vector paths, and scanned pages, which are
// images and need OCR that is not available here. When nothing extractable is
// found the error says so plainly instead of returning an empty success.

// pdfObject is a numbered indirect object: its dictionary text plus any stream
// bytes it carries. Page discovery goes through these rather than scanning the
// whole file, because font and encoding streams also contain byte strings that
// read as text but are not content.
type pdfObject struct {
	number int
	dict   string
	stream []byte
}

var (
	pdfObjHeader = regexp.MustCompile(`(\d+)\s+\d+\s+obj\b`)
	pdfPageRef   = regexp.MustCompile(`/Type\s*/Page([^s]|$)`)
	pdfContents  = regexp.MustCompile(`/Contents\s*(\[[^\]]*\]|(?:\d+\s+\d+\s+R))`)
	pdfRef       = regexp.MustCompile(`(\d+)\s+\d+\s+R`)
)

func pdfText(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory; name a file", filepath.Base(path))
	}
	if info.Size() > 100*1024*1024 {
		return "", fmt.Errorf("%s is over the 100 MB PDF limit", filepath.Base(path))
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("%PDF-")) {
		return "", fmt.Errorf("%s does not start with %%PDF, so it is not a PDF despite the extension", filepath.Base(path))
	}
	objects := pdfObjects(raw)
	streams := pdfContentStreams(objects)
	var parts []string
	for i, stream := range streams {
		text := pdfStreamText(stream)
		if strings.TrimSpace(text) == "" {
			continue
		}
		parts = append(parts, "--- page "+strconv.Itoa(i+1)+" ---\n"+strings.TrimSpace(text))
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("%s holds no extractable text: it may be scanned images, which need OCR that is not available here", filepath.Base(path))
	}
	return strings.Join(parts, "\n\n"), nil
}

// pdfObjects splits the file into numbered indirect objects. Anything outside
// an object (header, xref tables, trailer) carries no page text.
func pdfObjects(raw []byte) []pdfObject {
	var objects []pdfObject
	matches := pdfObjHeader.FindAllSubmatchIndex(raw, -1)
	for i, match := range matches {
		number, _ := strconv.Atoi(string(raw[match[2]:match[3]]))
		start := match[1]
		end := len(raw)
		if i+1 < len(matches) {
			end = matches[i+1][0]
		}
		body := raw[start:end]
		obj := pdfObject{number: number}
		if streamStart := bytes.Index(body, []byte("stream")); streamStart >= 0 {
			obj.dict = string(body[:streamStart])
			rest := body[streamStart+len("stream"):]
			rest = bytes.TrimPrefix(rest, []byte("\r\n"))
			rest = bytes.TrimPrefix(rest, []byte("\n"))
			rest = bytes.TrimPrefix(rest, []byte("\r"))
			if streamEnd := bytes.Index(rest, []byte("endstream")); streamEnd >= 0 {
				obj.stream = rest[:streamEnd]
			}
		} else {
			if endObj := bytes.Index(body, []byte("endobj")); endObj >= 0 {
				obj.dict = string(body[:endObj])
			} else {
				obj.dict = string(body)
			}
		}
		objects = append(objects, obj)
	}
	return objects
}

// pdfContentStreams resolves page /Contents references to their stream bytes.
// Page order is file order, which matches page order in every generator seen;
// the /Pages tree order would be exact but costs a full tree walk for no
// observed difference.
func pdfContentStreams(objects []pdfObject) [][]byte {
	byNumber := map[int]pdfObject{}
	for _, obj := range objects {
		byNumber[obj.number] = obj
	}
	var streams [][]byte
	for _, obj := range objects {
		if !pdfPageRef.MatchString(obj.dict) {
			continue
		}
		contents := pdfContents.FindStringSubmatch(obj.dict)
		if contents == nil {
			continue
		}
		for _, ref := range pdfRef.FindAllStringSubmatch(contents[1], -1) {
			number, _ := strconv.Atoi(ref[1])
			target, ok := byNumber[number]
			if !ok || len(target.stream) == 0 {
				continue
			}
			if decoded, ok := pdfDecode(target.dict, target.stream); ok {
				streams = append(streams, decoded)
			}
		}
	}
	// No pages resolved, which happens with object streams and other shapes
	// this reader does not walk. Fall back to every plausible content stream so
	// an unusual-but-simple file still reads, excluding the streams known to be
	// fonts and encodings rather than content.
	if len(streams) == 0 {
		for _, obj := range objects {
			if len(obj.stream) == 0 || pdfFontStream(obj.dict) {
				continue
			}
			if decoded, ok := pdfDecode(obj.dict, obj.stream); ok && pdfLooksLikeContent(decoded) {
				streams = append(streams, decoded)
			}
		}
	}
	return streams
}

// pdfFontStream names the streams that contain byte strings but no page text:
// font files, ToUnicode maps, and images. Parsing them as content is how a
// naive extractor ends up reporting "Adobe-Identity-UCS" as document text.
func pdfFontStream(dict string) bool {
	for _, marker := range []string{"/ToUnicode", "/FontFile", "/Subtype/Image", "/Subtype /Image", "/XObject"} {
		if strings.Contains(dict, marker) {
			return true
		}
	}
	return false
}

// pdfLooksLikeContent keeps the fallback honest: only streams carrying text
// showing operators are content. Without this the fallback would accept any
// decoded stream, including binary that happens to inflate.
func pdfLooksLikeContent(decoded []byte) bool {
	for _, op := range []string{"Tj", "TJ", "Tj\n", "'"} {
		if bytes.Contains(decoded, []byte(op)) {
			return true
		}
	}
	return false
}

// pdfDecode inflates a FlateDecode stream or passes through an uncompressed
// one. Anything else (images, LZW, ASCII85 chains) is skipped, because a
// partial decode is worse than an honest refusal: it returns fragments that
// read as content but are not.
func pdfDecode(dict string, stream []byte) ([]byte, bool) {
	filter := ""
	if idx := strings.Index(dict, "/Filter"); idx >= 0 {
		filter = dict[idx:]
		if end := strings.IndexAny(filter, ">/\n\r"); end > 0 && end < 120 {
			filter = filter[:end]
		} else if len(filter) > 120 {
			filter = filter[:120]
		}
	}
	if !strings.Contains(filter, "FlateDecode") {
		if strings.Contains(filter, "Filter") {
			return nil, false
		}
		return stream, true
	}
	for _, other := range []string{"ASCII85Decode", "LZWDecode", "DCTDecode", "CCITTFaxDecode", "Crypt"} {
		if strings.Contains(filter, other) {
			return nil, false
		}
	}
	reader, err := zlib.NewReader(bytes.NewReader(stream))
	if err != nil {
		return nil, false
	}
	defer reader.Close()
	decoded, err := io.ReadAll(io.LimitReader(reader, maxExtractedText*4))
	if err != nil {
		return nil, false
	}
	return decoded, true
}

// pdfStreamText walks a content stream and collects the strings shown by Tj,
// TJ, ' and " operators. Comments are skipped, and only strings adjacent to a
// showing operator are kept, so font names and metadata in the stream do not
// leak into the text.
func pdfStreamText(stream []byte) string {
	var out strings.Builder
	i := 0
	var pending []pdfString
	skip := func(n int) { i += n }
	for i < len(stream) {
		c := stream[i]
		switch {
		case c == '%':
			for i < len(stream) && stream[i] != '\n' {
				i++
			}
		case c == '(':
			text, next := pdfLiteral(stream, i)
			pending = append(pending, pdfString{text: text, hex: false})
			i = next
		case c == '<' && i+1 < len(stream) && stream[i+1] != '<':
			text, next := pdfHex(stream, i)
			pending = append(pending, pdfString{text: text, hex: true})
			i = next
		case c == '[':
			array, next := pdfArray(stream, i)
			pending = append(pending, array...)
			i = next
		case c == '\'' || c == '"':
			for _, s := range pending {
				out.WriteString(s.decoded())
			}
			out.WriteString("\n")
			pending = nil
			skip(1)
		case isOperatorStart(stream, i, "Tj"):
			for _, s := range pending {
				out.WriteString(s.decoded())
			}
			pending = nil
			skip(2)
		case isOperatorStart(stream, i, "TJ"):
			for _, s := range pending {
				out.WriteString(s.decoded())
			}
			pending = nil
			skip(2)
		case isOperatorStart(stream, i, "ET"):
			out.WriteString("\n")
			pending = nil
			skip(2)
		case c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\x00' || c == '\x0c':
			skip(1)
		case c == '/' || c == '>' || c == ']' || c == '{' || c == '}':
			// Names, dict ends and array ends carry no text. A pending string
			// followed by anything but a showing operator is metadata, so it
			// is dropped rather than kept just in case.
			if c == ']' {
				pending = nil
			}
			skip(1)
		default:
			// Numbers, names and other operators. A pending string survives
			// numbers (TJ spacing lives between its strings) but dies at any
			// other operator, which is what stops font names attaching to text.
			j := i
			for j < len(stream) && !isPDFDelimiter(stream[j]) {
				j++
			}
			token := string(stream[i:j])
			if _, err := strconv.ParseFloat(token, 64); err != nil {
				if token != "" && !isTJSpacing(token) {
					pending = nil
				}
			}
			i = j
		}
	}
	for _, s := range pending {
		out.WriteString(s.decoded())
	}
	return tidyBreaks(out.String())
}

// pdfString is one extracted operand. Hex strings from a ToUnicode-less font
// are glyph ids, not text, and decode as garbage; they are kept only inside TJ
// arrays where they usually are text, and literal strings are always kept.
type pdfString struct {
	text string
	hex  bool
	// kernedSpace marks strings separated by a TJ number gap wide enough to be
	// a word space rather than kerning. In thousandths of an em a normal space
	// is around 250, so 120 splits words from kerning without splitting "AV".
	kernedSpace bool
}

func (s pdfString) decoded() string {
	if s.kernedSpace {
		return " " + pdfDecodeBytes(s.text, s.hex)
	}
	return pdfDecodeBytes(s.text, s.hex)
}

// pdfArray parses a TJ array, marking word gaps. Elements alternate between
// strings and numbers; a number at or below -120 is a word space.
func pdfArray(stream []byte, start int) ([]pdfString, int) {
	var out []pdfString
	i := start + 1
	lastWasGap := false
	for i < len(stream) {
		c := stream[i]
		switch {
		case c == ']':
			return out, i + 1
		case c == '(':
			text, next := pdfLiteral(stream, i)
			out = append(out, pdfString{text: text, kernedSpace: lastWasGap})
			lastWasGap = false
			i = next
		case c == '<' && i+1 < len(stream) && stream[i+1] != '<':
			text, next := pdfHex(stream, i)
			out = append(out, pdfString{text: text, hex: true, kernedSpace: lastWasGap})
			lastWasGap = false
			i = next
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			i++
		default:
			j := i
			for j < len(stream) && !isPDFDelimiter(stream[j]) && stream[j] != ']' {
				j++
			}
			if num, err := strconv.ParseFloat(string(stream[i:j]), 64); err == nil && num <= -120 {
				lastWasGap = true
			}
			i = j
		}
	}
	return out, i
}

// pdfLiteral reads a parenthesised string with PDF escapes: \( \) \\, the
// single-letter escapes, octal bytes, and backslash-newline continuations.
func pdfLiteral(stream []byte, start int) (string, int) {
	var out strings.Builder
	i := start + 1
	depth := 1
	for i < len(stream) && depth > 0 {
		c := stream[i]
		switch {
		case c == '\\' && i+1 < len(stream):
			next := stream[i+1]
			switch {
			case next == 'n':
				out.WriteByte('\n')
				i += 2
			case next == 'r':
				out.WriteByte('\r')
				i += 2
			case next == 't':
				out.WriteByte('\t')
				i += 2
			case next == 'b':
				out.WriteByte('\b')
				i += 2
			case next == 'f':
				out.WriteByte('\f')
				i += 2
			case next == '(' || next == ')' || next == '\\':
				out.WriteByte(next)
				i += 2
			case next == '\r' || next == '\n':
				i += 2
				if next == '\r' && i < len(stream) && stream[i] == '\n' {
					i++
				}
			case next >= '0' && next <= '7':
				j := i + 1
				for j < len(stream) && j < i+4 && stream[j] >= '0' && stream[j] <= '7' {
					j++
				}
				var val int
				for _, digit := range stream[i+1 : j] {
					val = val*8 + int(digit-'0')
				}
				out.WriteByte(byte(val))
				i = j
			default:
				out.WriteByte(next)
				i += 2
			}
		case c == '(':
			depth++
			out.WriteByte(c)
			i++
		case c == ')':
			depth--
			if depth > 0 {
				out.WriteByte(c)
			}
			i++
		default:
			out.WriteByte(c)
			i++
		}
	}
	return out.String(), i
}

// pdfHex reads a hex string, ignoring whitespace and padding an odd tail. The
// result stays as raw bytes for WinAnsi decoding with the caller.
func pdfHex(stream []byte, start int) (string, int) {
	var digits []byte
	i := start + 1
	for i < len(stream) && stream[i] != '>' {
		c := stream[i]
		if isHexDigit(c) {
			digits = append(digits, c)
		}
		i++
	}
	raw := make([]byte, 0, (len(digits)+1)/2)
	for j := 0; j < len(digits); j += 2 {
		hi := hexVal(digits[j])
		lo := byte(0)
		if j+1 < len(digits) {
			lo = hexVal(digits[j+1])
		}
		raw = append(raw, hi<<4|lo)
	}
	return string(raw), i + 1
}

func isHexDigit(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func hexVal(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	default:
		return c - 'A' + 10
	}
}

func isPDFDelimiter(c byte) bool {
	switch c {
	case ' ', '\t', '\r', '\n', '\x00', '\x0c', '(', ')', '<', '>', '[', ']', '/', '%', '{', '}':
		return true
	}
	return false
}

func isOperatorStart(stream []byte, i int, op string) bool {
	if i+len(op) > len(stream) {
		return false
	}
	if string(stream[i:i+len(op)]) != op {
		return false
	}
	if i > 0 && !isPDFDelimiter(stream[i-1]) && stream[i-1] != '\'' && stream[i-1] != '"' {
		return false
	}
	if i+len(op) < len(stream) && !isPDFDelimiter(stream[i+len(op)]) {
		return false
	}
	return true
}

// isTJSpacing reports whether a token is one of the TJ-adjacent operators that
// must not kill a pending string. Only reached for non-numeric tokens.
func isTJSpacing(token string) bool {
	return token == "TJ" || token == "Tj"
}

// pdfDecodeBytes maps raw string bytes through WinAnsiEncoding, which is what
// an unmarked PDF font uses. ASCII passes through; the 0x80 range maps per the
// Windows code page, and the rest of high Latin-1 maps to itself.
func pdfDecodeBytes(text string, _ bool) string {
	var out strings.Builder
	for i := 0; i < len(text); i++ {
		b := text[i]
		if b < 0x80 {
			out.WriteByte(b)
			continue
		}
		if mapped, ok := winAnsi[b]; ok {
			out.WriteRune(mapped)
		} else {
			out.WriteRune(rune(b))
		}
	}
	return out.String()
}

// winAnsi maps the bytes where WinAnsiEncoding differs from Latin-1.
var winAnsi = map[byte]rune{
	0x80: 0x20AC, 0x82: 0x201A, 0x83: 0x0192, 0x84: 0x201E, 0x85: 0x2026,
	0x86: 0x2020, 0x87: 0x2021, 0x88: 0x02C6, 0x89: 0x2030, 0x8A: 0x0160,
	0x8B: 0x2039, 0x8C: 0x0152, 0x8E: 0x017D, 0x91: 0x2018, 0x92: 0x2019,
	0x93: 0x201C, 0x94: 0x201D, 0x95: 0x2022, 0x96: 0x2013, 0x97: 0x2014,
	0x98: 0x02DC, 0x99: 0x2122, 0x9A: 0x0161, 0x9B: 0x203A, 0x9C: 0x0153,
	0x9E: 0x017E, 0x9F: 0x0178,
}
