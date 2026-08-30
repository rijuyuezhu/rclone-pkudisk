package pkudisk

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/fs/object"
	"github.com/rclone/rclone/lib/dircache"
)

func TestPutUpdatesExistingObject(t *testing.T) {
	ctx := context.Background()
	var beginBody map[string]any
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/efast/v1/dir/list":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"dirs": []any{},
				"files": []map[string]any{{
					"docid": "gns://personal/existing", "name": "file.txt", "size": 3,
					"rev": "rev-old", "client_mtime": int64(1_577_934_245_000_000),
				}},
			})
		case "/api/efast/v1/file/osbeginupload":
			_ = json.NewDecoder(r.Body).Decode(&beginBody)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"docid": "gns://personal/existing", "rev": "rev-new",
				"authrequest": []string{"POST", server.URL + "/object-upload"},
			})
		case "/object-upload":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Fatal(err)
			}
			w.WriteHeader(http.StatusNoContent)
		case "/api/efast/v1/file/osendupload":
			_, _ = w.Write([]byte(`{}`))
		case "/api/efast/v1/file/metadata":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"docid": "gns://personal/existing", "file_name": "file.txt", "size": 7,
				"rev": "rev-new", "client_mtime": int64(1_609_646_706_654_321),
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	f := &Fs{
		name: "pku",
		opt:  Options{Enc: defaultEncoding},
		api:  newAPIClient(server.URL, &staticTokenProvider{token: "test-token"}),
	}
	f.dirCache = dircache.New("", virtualRootID, f)
	if err := f.dirCache.FindRoot(ctx, false); err != nil {
		t.Fatal(err)
	}
	f.dirCache.Put("Personal", "gns://personal")

	mtime := time.Unix(0, 1_609_646_706_654_321*int64(time.Microsecond)).UTC()
	src := object.NewStaticObjectInfo("Personal/file.txt", mtime, 7, true, nil, f)
	got, err := f.Put(ctx, strings.NewReader("updated"), src)
	if err != nil {
		t.Fatal(err)
	}
	if beginBody["docid"] != "gns://personal/existing" || beginBody["editedrev"] != "rev-old" {
		t.Fatalf("Put did not update existing revision: %#v", beginBody)
	}
	if _, ok := beginBody["name"]; ok {
		t.Fatalf("existing-file Put must not create a new named entry: %#v", beginBody)
	}
	if _, ok := beginBody["ondup"]; ok {
		t.Fatalf("existing-file Put must not use new-file duplicate handling: %#v", beginBody)
	}
	if got.Size() != 7 || !got.ModTime(ctx).Equal(mtime) {
		t.Fatalf("updated object size=%d mtime=%s", got.Size(), got.ModTime(ctx))
	}
}

func TestNilObjectString(t *testing.T) {
	var o *Object
	if got := o.String(); got != "<nil>" {
		t.Fatalf("nil Object.String() = %q, want <nil>", got)
	}
}
