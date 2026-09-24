package draftsvc

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"strings"
	"testing"
)

func TestParseProjectDraftFileSupportsPDFAndDOCX(t *testing.T) {
	pdfStream := new(bytes.Buffer)
	pdfWriter := zlib.NewWriter(pdfStream)
	_, _ = pdfWriter.Write([]byte("BT\n(Greenhouse monitoring) Tj\n(Complete document review) Tj\nET\n"))
	if err := pdfWriter.Close(); err != nil {
		t.Fatal(err)
	}
	pdf := append([]byte("%PDF-1.7\n1 0 obj\n<< /Filter /FlateDecode >>\nstream\n"), pdfStream.Bytes()...)
	pdf = append(pdf, []byte("\nendstream\nendobj\n")...)
	got, err := parseProjectDraftFile("brief.pdf", "application/pdf", pdf)
	if err != nil {
		t.Fatalf("pdf parse failed: %v", err)
	}
	if !strings.Contains(got, "Greenhouse monitoring") || !strings.Contains(got, "Complete document review") {
		t.Fatalf("unexpected pdf text: %q", got)
	}

	docx := makeTestDOCX(t, `<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>Greenhouse monitoring</w:t></w:r></w:p><w:p><w:r><w:t>Complete document review</w:t></w:r></w:p></w:body></w:document>`)
	got, err = parseProjectDraftFile("brief.docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", docx)
	if err != nil {
		t.Fatalf("docx parse failed: %v", err)
	}
	if !strings.Contains(got, "Greenhouse monitoring\nComplete document review") {
		t.Fatalf("unexpected docx text: %q", got)
	}
}

func TestParseProjectDraftFileRejectsMalformedSupportedFiles(t *testing.T) {
	for _, tc := range []struct {
		name, contentType string
		data              []byte
	}{
		{name: "pdf", contentType: "application/pdf", data: []byte("not a pdf")},
		{name: "docx", contentType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document", data: []byte("not a zip")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseProjectDraftFile("input."+tc.name, tc.contentType, tc.data); err == nil {
				t.Fatal("malformed supported file was accepted")
			}
		})
	}
}

func TestDraftFileParseableUsesFilenameForGenericOfficeMIME(t *testing.T) {
	for _, tc := range []struct {
		name, contentType string
	}{
		{name: "brief.pdf", contentType: "application/x-binary"},
		{name: "brief.docx", contentType: "application/x-binary"},
	} {
		if !draftFileParseable(tc.name, tc.contentType) {
			t.Fatalf("%s with %s should be parseable", tc.name, tc.contentType)
		}
	}
}

func makeTestDOCX(t *testing.T, documentXML string) []byte {
	t.Helper()
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(documentXML)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
