package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grassrootseconomics/storage-server/internal/storage"
)

type storedObject struct {
	name        string
	path        string
	contentType string
	size        int64
	data        []byte
}

type recordingStorage struct {
	mu      sync.Mutex
	uploads []storedObject
	err     error
}

func (s *recordingStorage) Upload(_ context.Context, fileName, path string, r io.Reader, size int64, contentType string) error {
	if s.err != nil {
		return s.err
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.uploads = append(s.uploads, storedObject{
		name:        fileName,
		path:        path,
		contentType: contentType,
		size:        size,
		data:        data,
	})
	return nil
}

func (s *recordingStorage) last() storedObject {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.uploads) == 0 {
		return storedObject{}
	}
	return s.uploads[len(s.uploads)-1]
}

func testAPI(store storage.Storage, overrides ...func(*APIOpts)) *API {
	o := APIOpts{
		EnableMetrics:   true,
		ListenAddress:   "127.0.0.1:0",
		MaxBodySize:     1 << 20,
		CORS:            []string{"https://sarafu.network", "http://localhost:3000"},
		UploadTimeout:   5 * time.Second,
		StorageProvider: store,
		Logg:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	for _, fn := range overrides {
		fn(&o)
	}
	return New(o)
}

func jpegBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func multipartBody(t *testing.T, fields map[string]string, filename string, file []byte) (*bytes.Buffer, string) {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	if filename != "" {
		fw, err := w.CreateFormFile("file", filename)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write(file); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return &body, w.FormDataContentType()
}

func decodeJSON(t *testing.T, res *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatalf("decode json: %v body=%q", err, res.Body.String())
	}
	return got
}

func TestUploadSuccessJSON(t *testing.T) {
	store := &recordingStorage{}
	api := testAPI(store)
	file := jpegBytes(t, 8, 8)
	body, ctype := multipartBody(t, map[string]string{
		"folder": "voucher",
		"name":   "bd10fd365101425f8bafeb6adfe8007c",
	}, "photo.jpg", file)

	req := httptest.NewRequest(http.MethodPost, "/v1/upload", body)
	req.Header.Set("Content-Type", ctype)
	res := httptest.NewRecorder()
	api.Handler().ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", res.Code, res.Body.String())
	}
	got := decodeJSON(t, res)
	if got["ok"] != true {
		t.Fatalf("ok = %v, want true", got["ok"])
	}
	payload, _ := got["payload"].(map[string]any)
	want := "https://content.sarafu.network/voucher/bd10fd365101425f8bafeb6adfe8007c.jpg"
	if payload["s3"] != want {
		t.Fatalf("s3 = %v, want %s", payload["s3"], want)
	}
	if len(store.uploads) != 1 {
		t.Fatalf("uploads = %d, want 1", len(store.uploads))
	}
	up := store.last()
	if up.path != "voucher" || up.name != "bd10fd365101425f8bafeb6adfe8007c.jpg" {
		t.Fatalf("stored key = %s/%s", up.path, up.name)
	}
	if up.contentType != "image/jpeg" {
		t.Fatalf("content type = %s", up.contentType)
	}
}

func TestUploadErrorEnvelopes(t *testing.T) {
	file := jpegBytes(t, 4, 4)

	cases := []struct {
		name   string
		fields map[string]string
		file   string
		data   []byte
		ctype  string
		body   io.Reader
		code   int
		err    string
	}{
		{
			name: "missing folder",
			fields: map[string]string{
				"name": "abc",
			},
			file: "p.jpg",
			data: file,
			code: http.StatusBadRequest,
			err:  "MISSING_FOLDER",
		},
		{
			name: "missing name uses folder error",
			fields: map[string]string{
				"folder": "voucher",
			},
			file: "p.jpg",
			data: file,
			code: http.StatusBadRequest,
			err:  "MISSING_FOLDER",
		},
		{
			name:   "missing file",
			fields: map[string]string{"folder": "voucher", "name": "abc"},
			code:   http.StatusBadRequest,
			err:    "MISSING_FILE",
		},
		{
			name:   "not multipart",
			ctype:  "application/json",
			body:   strings.NewReader(`{}`),
			code:   http.StatusBadRequest,
			err:    "NOT_MULTIPART",
		},
		{
			name:   "unsupported type",
			fields: map[string]string{"folder": "voucher", "name": "abc"},
			file:   "note.txt",
			data:   []byte("hello world this is not an image"),
			code:   http.StatusBadRequest,
			err:    "UNSUPPORTED_FILE_EXTENSION",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := testAPI(&recordingStorage{})
			var (
				body  io.Reader
				ctype string
			)
			if tc.body != nil {
				body, ctype = tc.body, tc.ctype
			} else {
				b, ct := multipartBody(t, tc.fields, tc.file, tc.data)
				body, ctype = b, ct
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/upload", body)
			req.Header.Set("Content-Type", ctype)
			res := httptest.NewRecorder()
			api.Handler().ServeHTTP(res, req)

			if res.Code != tc.code {
				t.Fatalf("status = %d, want %d body=%s", res.Code, tc.code, res.Body.String())
			}
			got := decodeJSON(t, res)
			if got["ok"] != false {
				t.Fatalf("ok = %v, want false", got["ok"])
			}
			if got["code"] != tc.err {
				t.Fatalf("code = %v, want %s", got["code"], tc.err)
			}
		})
	}
}

func TestUploadBodyLimit(t *testing.T) {
	api := testAPI(&recordingStorage{}, func(o *APIOpts) {
		o.MaxBodySize = 256
	})
	file := jpegBytes(t, 64, 64)
	if len(file) < 256 {
		t.Fatalf("fixture too small to exceed limit: %d", len(file))
	}
	body, ctype := multipartBody(t, map[string]string{
		"folder": "voucher",
		"name":   "too-big",
	}, "photo.jpg", file)

	req := httptest.NewRequest(http.MethodPost, "/v1/upload", body)
	req.Header.Set("Content-Type", ctype)
	res := httptest.NewRecorder()
	api.Handler().ServeHTTP(res, req)

	if res.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 body=%s", res.Code, res.Body.String())
	}
	got := decodeJSON(t, res)
	if got["code"] != "FILE_SIZE_LIMIT_EXCEEDED" {
		t.Fatalf("code = %v, want FILE_SIZE_LIMIT_EXCEEDED", got["code"])
	}
}

func TestCORSPreflightConfiguredOrigin(t *testing.T) {
	api := testAPI(&recordingStorage{})
	req := httptest.NewRequest(http.MethodOptions, "/v1/upload", nil)
	req.Header.Set("Origin", "https://sarafu.network")
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	req.Header.Set("Access-Control-Request-Headers", "content-type")
	res := httptest.NewRecorder()
	api.Handler().ServeHTTP(res, req)

	if got := res.Header().Get("Access-Control-Allow-Origin"); got != "https://sarafu.network" {
		t.Fatalf("allow origin = %q", got)
	}
	if got := res.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Fatalf("allow credentials = %q", got)
	}
	allowMethods := res.Header().Get("Access-Control-Allow-Methods")
	if !strings.Contains(allowMethods, http.MethodPost) {
		t.Fatalf("allow methods = %q, want POST", allowMethods)
	}
}

func TestCORSPreflightRejectsUnknownOrigin(t *testing.T) {
	api := testAPI(&recordingStorage{})
	req := httptest.NewRequest(http.MethodOptions, "/v1/upload", nil)
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	req.Header.Set("Access-Control-Request-Headers", "content-type")
	res := httptest.NewRecorder()
	api.Handler().ServeHTTP(res, req)

	if got := res.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("allow origin = %q, want empty", got)
	}
}

func TestMetricsRouteEnabled(t *testing.T) {
	api := testAPI(&recordingStorage{})
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	res := httptest.NewRecorder()
	api.Handler().ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}
	if !strings.Contains(res.Body.String(), "go_") && !strings.Contains(res.Body.String(), "process_") {
		t.Fatalf("expected prometheus metrics, got %q", res.Body.String())
	}
}

func TestMetricsRouteDisabled(t *testing.T) {
	api := testAPI(&recordingStorage{}, func(o *APIOpts) {
		o.EnableMetrics = false
	})
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	res := httptest.NewRecorder()
	api.Handler().ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}
	if res.Body.Len() != 0 {
		t.Fatalf("disabled metrics wrote %q", res.Body.String())
	}
}

func TestGracefulShutdown(t *testing.T) {
	api := testAPI(&recordingStorage{})
	errCh := make(chan error, 1)
	go func() {
		errCh <- api.Start()
	}()

	select {
	case err := <-errCh:
		t.Fatalf("server exited early: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := api.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("start err = %v, want ErrServerClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not shut down")
	}
}
