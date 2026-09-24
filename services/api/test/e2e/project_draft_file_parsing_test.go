package e2e_test

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"database/sql"
	"encoding/ascii85"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestPrivateProjectDraftOfficeFilesParsePrivately(t *testing.T) {
	f, _, session, _ := newProjectDraftFixture(t)
	d := f.request(t, http.MethodPost, f.base, map[string]any{
		"name": "文件解析验收", "goal": "保留真实解析状态", "chat_session_id": session,
	}, http.StatusCreated)
	path := f.base + "/" + d["id"].(string)
	before := projectBusinessCounts(t, f.env)
	for _, tc := range []struct {
		name, contentType string
		malformed         bool
	}{
		{"brief.pdf", "application/pdf", false},
		{"brief.docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", false},
		{"broken.pdf", "application/pdf", true},
		{"broken.docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := []byte("not a document")
			if !tc.malformed {
				data = projectDraftOfficeFixture(t, tc.name)
			}
			start := f.request(t, http.MethodPost, path+"/files/initiate-upload", map[string]any{
				"file_name": tc.name, "content_type": tc.contentType, "file_size": len(data),
			}, http.StatusCreated)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, start["upload_url"].(string), bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", tc.contentType)
			resp := mustDo(t, f.client, req)
			resp.Body.Close()
			assertStatus(t, resp, http.StatusOK)
			completed := f.request(t, http.MethodPost, path+"/files/complete-upload", map[string]any{"file_id": start["file_id"]}, http.StatusOK)
			var extracted sql.NullString
			if err := f.env.db.Get(&extracted, `SELECT extracted_text FROM project_draft_files WHERE file_id=$1`, start["file_id"]); err != nil {
				t.Fatal(err)
			}
			if tc.malformed {
				if completed["processing_status"] != "failed" || completed["processing_error"] == nil || extracted.Valid {
					t.Fatalf("invalid file was marked readable: %#v", completed)
				}
			} else if completed["processing_status"] != "ready" || completed["upload_status"] != "uploaded" || !strings.Contains(extracted.String, "Greenhouse monitoring") || !strings.Contains(extracted.String, "Complete document review") {
				t.Fatalf("valid document was not extracted and persisted: %#v text=%q", completed, extracted.String)
			}
		})
	}
	listed := f.request(t, http.MethodGet, path+"/files", nil, http.StatusOK)
	if len(listed["items"].([]any)) != 4 {
		t.Fatalf("file states did not survive reload: %#v", listed)
	}
	if after := projectBusinessCounts(t, f.env); !reflect.DeepEqual(before, after) {
		t.Fatalf("private uploads created public business objects: before=%v after=%v", before, after)
	}
	seedProjectAdminUser(t, f.env, "office-file-other", "password1")
	otherClient, otherToken := projectAdminLogin(t, f.env, "office-file-other", "password1")
	for _, file := range listed["items"].([]any) {
		req := mustRequest(t.Context(), t, http.MethodPost, f.env.base+path+"/files/complete-upload", jsonBody(t, map[string]any{"file_id": file.(map[string]any)["file_id"]}))
		req.Header.Set("Authorization", "Bearer "+otherToken)
		req.Header.Set("Content-Type", "application/json")
		resp := mustDo(t, otherClient, req)
		resp.Body.Close()
		assertStatus(t, resp, http.StatusNotFound)
	}
}

func projectDraftOfficeFixture(t *testing.T, name string) []byte {
	t.Helper()
	if strings.HasSuffix(name, ".pdf") {
		var compressed bytes.Buffer
		writer := zlib.NewWriter(&compressed)
		if _, err := writer.Write([]byte("BT\n(Greenhouse monitoring) Tj\n(Complete document review) Tj\nET\n")); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		encoded := make([]byte, ascii85.MaxEncodedLen(compressed.Len()))
		length := ascii85.Encode(encoded, compressed.Bytes())
		pdf := []byte("%PDF-1.7\n1 0 obj\n<< /Filter [ /ASCII85Decode /FlateDecode ] >>\nstream\n")
		pdf = append(pdf, encoded[:length]...)
		pdf = append(pdf, []byte("~>\nendstream\nendobj\n")...)
		return pdf
	}
	var out bytes.Buffer
	writer := zip.NewWriter(&out)
	file, err := writer.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte(`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>Greenhouse monitoring</w:t></w:r></w:p><w:p><w:r><w:t>Complete document review</w:t></w:r></w:p></w:body></w:document>`)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
