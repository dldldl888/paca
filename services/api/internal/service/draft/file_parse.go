package draftsvc

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"encoding/xml"
	"errors"
	"io"
	"path"
	"strings"
)

var errUnsupportedDraftFile = errors.New("unsupported project draft file format")

// ponytail: keep parsing in the standard library; scanned PDFs, embedded
// images, and font-specific glyph maps remain explicit unsupported cases.
func parseProjectDraftFile(fileName, contentType string, data []byte) (string, error) {
	contentType = strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	ext := strings.ToLower(path.Ext(fileName))
	switch {
	case contentType == "text/plain" || contentType == "text/markdown":
		return string(data), nil
	case contentType == "application/pdf" || (contentType == "application/octet-stream" && ext == ".pdf") || ext == ".pdf":
		return parsePDFText(data)
	case contentType == "application/vnd.openxmlformats-officedocument.wordprocessingml.document" || (contentType == "application/octet-stream" && ext == ".docx") || ext == ".docx":
		return parseDOCXText(data)
	default:
		return "", errUnsupportedDraftFile
	}
}

func parsePDFText(data []byte) (string, error) {
	if !bytes.HasPrefix(data, []byte("%PDF-")) {
		return "", errors.New("invalid PDF header")
	}
	var text strings.Builder
	for search := 0; search < len(data); {
		rel := bytes.Index(data[search:], []byte("stream"))
		if rel < 0 {
			break
		}
		streamStart := search + rel + len("stream")
		if streamStart < len(data) && data[streamStart] == '\r' {
			streamStart++
		}
		if streamStart < len(data) && data[streamStart] == '\n' {
			streamStart++
		}
		endRel := bytes.Index(data[streamStart:], []byte("endstream"))
		if endRel < 0 {
			return "", errors.New("PDF stream is incomplete")
		}
		streamEnd := streamStart + endRel
		stream := data[streamStart:streamEnd]
		dictionaryStart := bytes.LastIndex(data[:search+rel], []byte("<<"))
		if dictionaryStart < 0 {
			dictionaryStart = search + rel - 512
			if dictionaryStart < 0 {
				dictionaryStart = 0
			}
		}
		dictionary := data[dictionaryStart : search+rel]
		if bytes.Contains(dictionary, []byte("/ASCII85Decode")) {
			var decodeErr error
			stream, decodeErr = ascii85Decode(stream)
			if decodeErr != nil {
				return "", errors.New("PDF ASCII85 stream is invalid")
			}
		}
		if bytes.Contains(dictionary, []byte("/FlateDecode")) {
			reader, err := zlib.NewReader(bytes.NewReader(stream))
			if err != nil {
				return "", errors.New("PDF compressed stream is invalid")
			}
			decoded, readErr := io.ReadAll(io.LimitReader(reader, MaxBody+1))
			closeErr := reader.Close()
			if readErr != nil || closeErr != nil || len(decoded) > MaxBody {
				return "", errors.New("PDF stream exceeds the parsing limit")
			}
			stream = decoded
		}
		if part := pdfTextOperators(stream); part != "" {
			if text.Len() > 0 {
				text.WriteByte('\n')
			}
			text.WriteString(part)
		}
		search = streamEnd + len("endstream")
	}
	if text.Len() == 0 {
		text.WriteString(pdfTextOperators(data))
	}
	if text.Len() == 0 {
		return "", errors.New("PDF contains no readable text")
	}
	return strings.TrimSpace(text.String()), nil
}

func ascii85Decode(data []byte) ([]byte, error) {
	out := make([]byte, 0, len(data)*4/5)
	var tuple [5]byte
	n := 0
	terminated := false
	flush := func(count int) {
		value := uint32(0)
		for i := 0; i < 5; i++ {
			value = value*85 + uint32(tuple[i])
		}
		for i := 0; i < count-1; i++ {
			out = append(out, byte(value>>uint(24-8*i)))
		}
	}
	for i := 0; i < len(data); i++ {
		char := data[i]
		if char == '~' {
			if i+1 >= len(data) || data[i+1] != '>' {
				return nil, errors.New("invalid ASCII85 terminator")
			}
			i++
			terminated = true
			break
		}
		if char == ' ' || char == '\t' || char == '\r' || char == '\n' {
			continue
		}
		if char == 'z' {
			if n != 0 {
				return nil, errors.New("invalid ASCII85 zero tuple")
			}
			out = append(out, 0, 0, 0, 0)
			continue
		}
		if char < '!' || char > 'u' {
			return nil, errors.New("invalid ASCII85 digit")
		}
		tuple[n] = char - '!'
		n++
		if n == 5 {
			flush(5)
			n = 0
		}
	}
	if n == 1 {
		return nil, errors.New("truncated ASCII85 tuple")
	}
	if !terminated {
		return nil, errors.New("missing ASCII85 terminator")
	}
	if n > 1 {
		for i := n; i < 5; i++ {
			tuple[i] = 'u' - '!'
		}
		flush(n)
	}
	return out, nil
}

func pdfTextOperators(stream []byte) string {
	var out strings.Builder
	for cursor := 0; cursor < len(stream); {
		start := bytes.Index(stream[cursor:], []byte("BT"))
		if start < 0 {
			break
		}
		start += cursor + 2
		end := bytes.Index(stream[start:], []byte("ET"))
		if end < 0 {
			break
		}
		end += start
		for i := start; i < end; i++ {
			switch stream[i] {
			case '(':
				value, next, ok := pdfLiteral(stream, i)
				if !ok {
					continue
				}
				appendPDFText(&out, value)
				i = next - 1
			case '<':
				if i+1 < end && stream[i+1] != '<' {
					value, next, ok := pdfHexLiteral(stream, i)
					if ok {
						appendPDFText(&out, value)
						i = next - 1
					}
				}
			}
		}
		cursor = end + 2
	}
	return strings.TrimSpace(out.String())
}

func appendPDFText(out *strings.Builder, value []byte) {
	if len(value) == 0 {
		return
	}
	if out.Len() > 0 {
		out.WriteByte(' ')
	}
	out.Write(value)
}

func pdfLiteral(data []byte, start int) ([]byte, int, bool) {
	depth := 0
	var out bytes.Buffer
	for i := start; i < len(data); i++ {
		switch data[i] {
		case '(':
			if depth > 0 {
				out.WriteByte(data[i])
			}
			depth++
		case ')':
			depth--
			if depth == 0 {
				return out.Bytes(), i + 1, true
			}
			out.WriteByte(data[i])
		case '\\':
			if depth == 0 || i+1 >= len(data) {
				return nil, 0, false
			}
			i++
			switch data[i] {
			case 'n':
				out.WriteByte('\n')
			case 'r':
				out.WriteByte('\r')
			case 't':
				out.WriteByte('\t')
			case 'b':
				out.WriteByte('\b')
			case 'f':
				out.WriteByte('\f')
			case '\r':
				if i+1 < len(data) && data[i+1] == '\n' {
					i++
				}
			case '\n':
			default:
				if data[i] >= '0' && data[i] <= '7' {
					value := int(data[i] - '0')
					for n := 0; n < 2 && i+1 < len(data) && data[i+1] >= '0' && data[i+1] <= '7'; n++ {
						i++
						value = value*8 + int(data[i]-'0')
					}
					out.WriteByte(byte(value))
				} else {
					out.WriteByte(data[i])
				}
			}
		default:
			if depth > 0 {
				out.WriteByte(data[i])
			}
		}
	}
	return nil, 0, false
}

func pdfHexLiteral(data []byte, start int) ([]byte, int, bool) {
	end := bytes.IndexByte(data[start+1:], '>')
	if end < 0 {
		return nil, 0, false
	}
	end += start + 1
	value := make([]byte, 0, (end-start)/2)
	var nibble byte
	for _, char := range data[start+1 : end] {
		if char == ' ' || char == '\t' || char == '\r' || char == '\n' {
			continue
		}
		digit, ok := pdfHexDigit(char)
		if !ok {
			return nil, 0, false
		}
		if len(value)%2 == 0 {
			nibble = digit << 4
			value = append(value, nibble)
		} else {
			value[len(value)-1] |= digit
		}
	}
	if len(value)%2 == 1 {
		value[len(value)-1] |= 0
	}
	return value, end + 1, true
}

func pdfHexDigit(char byte) (byte, bool) {
	switch {
	case char >= '0' && char <= '9':
		return char - '0', true
	case char >= 'a' && char <= 'f':
		return char - 'a' + 10, true
	case char >= 'A' && char <= 'F':
		return char - 'A' + 10, true
	default:
		return 0, false
	}
}

func parseDOCXText(data []byte) (string, error) {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", errors.New("invalid DOCX archive")
	}
	var document *zip.File
	var total uint64
	for _, file := range reader.File {
		total += file.UncompressedSize64
		if total > MaxBody {
			return "", errors.New("DOCX contents exceed the parsing limit")
		}
		if file.Name == "word/document.xml" {
			document = file
		}
	}
	if document == nil {
		return "", errors.New("DOCX document.xml is missing")
	}
	fileReader, err := document.Open()
	if err != nil {
		return "", errors.New("DOCX document.xml cannot be opened")
	}
	defer fileReader.Close()
	decoder := xml.NewDecoder(io.LimitReader(fileReader, MaxBody+1))
	var out strings.Builder
	inText := false
	for {
		token, tokenErr := decoder.Token()
		if tokenErr == io.EOF {
			break
		}
		if tokenErr != nil {
			return "", errors.New("DOCX document.xml is invalid")
		}
		switch value := token.(type) {
		case xml.StartElement:
			switch value.Name.Local {
			case "t":
				inText = true
			case "br", "cr":
				out.WriteByte('\n')
			case "tab":
				out.WriteByte('\t')
			}
		case xml.CharData:
			if inText {
				out.Write([]byte(value))
			}
		case xml.EndElement:
			switch value.Name.Local {
			case "t":
				inText = false
			case "p":
				if out.Len() > 0 && out.String()[out.Len()-1] != '\n' {
					out.WriteByte('\n')
				}
			}
		}
		if out.Len() > MaxBody {
			return "", errors.New("DOCX text exceeds the parsing limit")
		}
	}
	text := strings.TrimSpace(out.String())
	if text == "" {
		return "", errors.New("DOCX contains no readable text")
	}
	return text, nil
}
