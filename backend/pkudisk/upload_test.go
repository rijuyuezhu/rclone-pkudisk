package pkudisk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSingleUploadStreamsSignedForm(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/efast/v1/file/osbeginupload":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"docid": "gns://lib/file", "rev": "rev-1",
				"authrequest": []string{"POST", server.URL + "/object-upload", "policy: signed-policy"},
			})
		case "/object-upload":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Fatal(err)
			}
			if r.FormValue("policy") != "signed-policy" {
				t.Fatalf("policy = %q", r.FormValue("policy"))
			}
			file, _, err := r.FormFile("file")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			data, _ := io.ReadAll(file)
			if string(data) != "hello" {
				t.Fatalf("uploaded data = %q", data)
			}
			w.WriteHeader(http.StatusNoContent)
		case "/api/efast/v1/file/osendupload":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		case "/api/efast/v1/file/metadata":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"docid": "gns://lib/file", "file_name": "hello.txt", "size": 5, "rev": "rev-1", "modified": int64(1_788_000_000_000_000),
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newAPIClient(server.URL, &staticTokenProvider{token: "test-token"})
	meta, err := client.uploadSingle(context.Background(), "gns://lib", "hello.txt", "", "", 5, time.Unix(1_788_000_000, 0), strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if meta.DocID != "gns://lib/file" || meta.Size != 5 {
		t.Fatalf("metadata = %#v", meta)
	}
}

func TestExistingFileUploadCreatesVersionWithEditedRev(t *testing.T) {
	var beginBody map[string]any
	var endBody map[string]any
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/efast/v1/file/osbeginupload":
			_ = json.NewDecoder(r.Body).Decode(&beginBody)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"docid": "gns://lib/existing", "rev": "rev-new",
				"authrequest": []string{"POST", server.URL + "/object-upload"},
			})
		case "/object-upload":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Fatal(err)
			}
			w.WriteHeader(http.StatusNoContent)
		case "/api/efast/v1/file/osendupload":
			_ = json.NewDecoder(r.Body).Decode(&endBody)
			_, _ = w.Write([]byte(`{}`))
		case "/api/efast/v1/file/metadata":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"docid": "gns://lib/existing", "file_name": "file.txt", "size": 7, "rev": "rev-new",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newAPIClient(server.URL, &staticTokenProvider{token: "test-token"})
	_, err := client.uploadSingle(context.Background(), "gns://lib", "file.txt", "gns://lib/existing", "rev-old", 7, time.Now(), strings.NewReader("updated"))
	if err != nil {
		t.Fatal(err)
	}
	if beginBody["docid"] != "gns://lib/existing" || beginBody["editedrev"] != "rev-old" {
		t.Fatalf("edit begin body = %#v", beginBody)
	}
	if _, ok := beginBody["name"]; ok {
		t.Fatalf("edit begin must not send name: %#v", beginBody)
	}
	if _, ok := beginBody["ondup"]; ok {
		t.Fatalf("edit begin must not send ondup: %#v", beginBody)
	}
	if endBody["editedrev"] != "rev-old" || endBody["rev"] != "rev-new" {
		t.Fatalf("edit end body = %#v", endBody)
	}
}

func TestMultipartUploadStreamsPartsAndCompletes(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 100)
	var gotParts [][]byte
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/efast/v1/file/osoption":
			_ = json.NewEncoder(w).Encode(map[string]any{"partminsize": 8, "partmaxsize": 64, "partmaxnum": 2})
		case "/api/efast/v1/file/osinitmultiupload":
			_ = json.NewEncoder(w).Encode(map[string]any{"docid": "gns://lib/multi", "rev": "rev-m", "uploadid": "upload-1"})
		case "/api/efast/v1/file/osuploadpart":
			_ = json.NewEncoder(w).Encode(map[string]any{"authrequests": map[string]any{
				"1": []string{"PUT", server.URL + "/part/1", "Authorization: signed-1", "x-as-userid: ignored"},
				"2": []string{"PUT", server.URL + "/part/2", "Authorization: signed-2"},
			}})
		case "/part/1", "/part/2":
			data, _ := io.ReadAll(r.Body)
			gotParts = append(gotParts, data)
			if strings.HasSuffix(r.URL.Path, "/1") && r.Header.Get("Authorization") != "signed-1" {
				t.Fatalf("part 1 auth = %q", r.Header.Get("Authorization"))
			}
			if r.Header.Get("x-as-userid") != "" {
				t.Fatal("x-as-userid leaked to object storage")
			}
			w.Header().Set("ETag", fmt.Sprintf("\"etag-%d\"", len(gotParts)))
		case "/api/efast/v1/file/oscompleteupload":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			parts := body["partinfo"].(map[string]any)
			if len(parts) != 2 {
				t.Fatalf("partinfo count = %d", len(parts))
			}
			completion := "--boundary\n<CompleteMultipartUpload/>\n--boundary\n" +
				`{"authrequest":["POST","` + server.URL + `/complete","Content-Type: application/xml","x-as-userid: ignored"]}` +
				"\n--boundary"
			_, _ = w.Write([]byte(completion))
		case "/complete":
			data, _ := io.ReadAll(r.Body)
			if string(data) != "<CompleteMultipartUpload/>" {
				t.Fatalf("completion body = %q", data)
			}
			if r.Header.Get("x-as-userid") != "" {
				t.Fatal("x-as-userid leaked on completion")
			}
		case "/api/efast/v1/file/osendupload":
			_, _ = w.Write([]byte(`{}`))
		case "/api/efast/v1/file/metadata":
			_ = json.NewEncoder(w).Encode(map[string]any{"docid": "gns://lib/multi", "file_name": "large.bin", "size": 100, "rev": "rev-m"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newAPIClient(server.URL, &staticTokenProvider{token: "test-token"})
	meta, err := client.uploadMultipart(context.Background(), "gns://lib", "large.bin", "", "", int64(len(payload)), time.Now(), bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if meta.Size != int64(len(payload)) {
		t.Fatalf("metadata size = %d", meta.Size)
	}
	if len(gotParts) != 2 || len(gotParts[0]) != 64 || len(gotParts[1]) != 36 {
		t.Fatalf("part sizes = %v", []string{strconv.Itoa(len(gotParts[0])), strconv.Itoa(len(gotParts[1]))})
	}
	if !bytes.Equal(append(append([]byte{}, gotParts[0]...), gotParts[1]...), payload) {
		t.Fatal("multipart payload changed")
	}
}
