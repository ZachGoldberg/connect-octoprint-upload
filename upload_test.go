package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const goodToken = "connect-credential-for-tests"

// fakeConnect is the part of Prusa Connect's HTTP API the module calls.
type fakeConnect struct {
	t        *testing.T
	printers []device

	mu          sync.Mutex
	stored      map[string][]byte // storage/path -> bytes
	storedHash  map[string]string
	prints      []printRequest
	printDevice []string
	refuseStart bool
	status      int // when set, every call answers this status
}

func newFakeConnect(t *testing.T, printers ...device) (*fakeConnect, *httptest.Server) {
	t.Helper()

	fake := &fakeConnect{t: t, printers: printers, stored: map[string][]byte{}, storedHash: map[string]string{}}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)

	return fake, server
}

func (f *fakeConnect) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+goodToken {
		problem(w, http.StatusUnauthorized, "core.auth/UNAUTHENTICATED", "No valid credential")

		return
	}

	if f.status != 0 {
		problem(w, f.status, "core.auth/ACCESS_DENIED", "The caller may not do that")

		return
	}

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/devices":
		writeJSON(w, http.StatusOK, map[string]any{"items": f.printers})
	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/v1/storages/") && strings.HasSuffix(r.URL.Path, "/content"):
		storage := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/storages/"), "/content")
		target := r.URL.Query().Get("path")

		if r.ContentLength < 0 {
			problem(w, http.StatusLengthRequired, "core.validation/LENGTH_REQUIRED", "An upload names its length")

			return
		}

		hash := sha256.New()

		body, err := io.ReadAll(io.TeeReader(r.Body, hash))
		if err != nil {
			problem(w, http.StatusBadRequest, "core.validation/BODY", err.Error())

			return
		}

		f.mu.Lock()
		f.stored[storage+"/"+target] = body
		f.storedHash[storage+"/"+target] = hex.EncodeToString(hash.Sum(nil))
		f.mu.Unlock()

		writeJSON(w, http.StatusOK, map[string]any{"path": target, "bytes": len(body)})
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/v1/devices/") && strings.HasSuffix(r.URL.Path, "/print"):
		var request printRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			problem(w, http.StatusBadRequest, "core.validation/BODY", err.Error())

			return
		}

		f.mu.Lock()
		f.prints = append(f.prints, request)
		f.printDevice = append(f.printDevice, strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/devices/"), "/print"))
		refuse := f.refuseStart
		f.mu.Unlock()

		command := map[string]any{"ok": !refuse}
		if refuse {
			command["machine_reason"] = "a print is already running"
		}

		writeJSON(w, http.StatusOK, map[string]any{"path": request.Path, "command": command})
	default:
		problem(w, http.StatusNotFound, "core.validation/ROUTE_NOT_FOUND", "No operation is published at that path")
	}
}

func problem(w http.ResponseWriter, status int, code, title string) {
	writeJSON(w, status, map[string]any{"code": code, "title": title, "status": status})
}

// newTestModule is a configured module talking to the fake Connect, with its
// upload routes served by an httptest server.
func newTestModule(t *testing.T, connectURL string) (*module, *httptest.Server, string) {
	t.Helper()

	dataDir := t.TempDir()

	connect, err := newConnectClient(connectURL)
	if err != nil {
		t.Fatal(err)
	}

	held := &module{
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		settings: Settings{ConnectURL: connectURL, Folder: "slicer-uploads"}.withDefaults(),
		connect:  connect,
		dataDir:  dataDir,
	}

	server := httptest.NewServer(held.uploadRoutes())
	t.Cleanup(server.Close)

	return held, server, dataDir
}

// prusaSlicerUpload builds the body PrusaSlicer 2.9's OctoPrint host sends:
// form fields "print", "path" and then the file part "file", in that order
// (src/slic3r/Utils/OctoPrint.cpp, OctoPrint::upload_inner_with_host).
func prusaSlicerUpload(t *testing.T, print bool, folder, filename string, content []byte) (*bytes.Buffer, string) {
	t.Helper()

	var body bytes.Buffer

	writer := multipart.NewWriter(&body)

	printValue := "false"
	if print {
		printValue = "true"
	}

	if err := writer.WriteField("print", printValue); err != nil {
		t.Fatal(err)
	}

	if err := writer.WriteField("path", folder); err != nil {
		t.Fatal(err)
	}

	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}

	if _, err = part.Write(content); err != nil {
		t.Fatal(err)
	}

	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}

	return &body, writer.FormDataContentType()
}

func send(t *testing.T, method, url, token, contentType string, body io.Reader) (int, string) {
	t.Helper()

	request, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}

	if token != "" {
		request.Header.Set("X-Api-Key", token)
	}

	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()

	answer, _ := io.ReadAll(response.Body)

	return response.StatusCode, string(answer)
}

var coreOne = device{ID: "00000000-0000-4000-8000-000000000001", Name: "COREONE 10000-0000000000000000"}

func TestVersionAnswersAsOctoPrint(t *testing.T) {
	_, connect := newFakeConnect(t, coreOne)
	_, server, _ := newTestModule(t, connect.URL)

	for _, address := range []string{"/api/version", "/core-one/api/version", "/COREONE/api/version", "/" + coreOne.ID + "/api/version"} {
		status, body := send(t, http.MethodGet, server.URL+address, goodToken, "", nil)
		if status != http.StatusOK {
			t.Fatalf("%s: status %d: %s", address, status, body)
		}

		var answer map[string]string
		if err := json.Unmarshal([]byte(body), &answer); err != nil {
			t.Fatal(err)
		}

		// PrusaSlicer's OctoPrint::validate_version_text: the text must start with "OctoPrint".
		if !strings.HasPrefix(answer["text"], "OctoPrint") || !strings.Contains(answer["text"], coreOne.Name) {
			t.Fatalf("%s: text %q", address, answer["text"])
		}
	}
}

func TestCredentialIsRequiredAndPassedThrough(t *testing.T) {
	_, connect := newFakeConnect(t, coreOne)
	_, server, _ := newTestModule(t, connect.URL)

	status, body := send(t, http.MethodGet, server.URL+"/api/version", "", "", nil)
	if status != http.StatusUnauthorized || !strings.Contains(body, "API credentials") {
		t.Fatalf("no key: %d %s", status, body)
	}

	status, body = send(t, http.MethodGet, server.URL+"/api/version", "not-a-credential", "", nil)
	if status != http.StatusUnauthorized || !strings.Contains(body, "does not accept this API key") {
		t.Fatalf("wrong key: %d %s", status, body)
	}
}

func TestPrinterSelection(t *testing.T) {
	other := device{ID: "11111111-2222-3333-4444-555555555555", Name: "MK4S 12345-0000"}

	_, connect := newFakeConnect(t, coreOne, other)
	_, server, _ := newTestModule(t, connect.URL)

	status, body := send(t, http.MethodGet, server.URL+"/api/version", goodToken, "", nil)
	if status != http.StatusNotFound || !strings.Contains(body, "put one in the host address") {
		t.Fatalf("two printers, none named: %d %s", status, body)
	}

	status, body = send(t, http.MethodGet, server.URL+"/mk4s/api/version", goodToken, "", nil)
	if status != http.StatusOK || !strings.Contains(body, other.Name) {
		t.Fatalf("by prefix: %d %s", status, body)
	}

	status, body = send(t, http.MethodGet, server.URL+"/xl/api/version", goodToken, "", nil)
	if status != http.StatusNotFound || !strings.Contains(body, coreOne.Name) {
		t.Fatalf("unknown printer: %d %s", status, body)
	}
}

func TestUploadOnlyStoresTheFile(t *testing.T) {
	fake, connect := newFakeConnect(t, coreOne)
	_, server, dataDir := newTestModule(t, connect.URL)

	content := []byte("; sliced by a test\nG28\n")
	body, contentType := prusaSlicerUpload(t, false, "", "bracket_0.4n_0.2mm_PLA_COREONE_12m.bgcode", content)

	status, answer := send(t, http.MethodPost, server.URL+"/core-one/api/files/local", goodToken, contentType, body)
	if status != http.StatusCreated {
		t.Fatalf("status %d: %s", status, answer)
	}

	if got := fake.stored["local/slicer-uploads/bracket_0.4n_0.2mm_PLA_COREONE_12m.bgcode"]; !bytes.Equal(got, content) {
		t.Fatalf("stored %q", got)
	}

	if len(fake.prints) != 0 {
		t.Fatalf("an upload without print started a print: %+v", fake.prints)
	}

	assertNoLeftovers(t, dataDir)
}

func TestUploadAndPrintReplacesOnThePrinter(t *testing.T) {
	fake, connect := newFakeConnect(t, coreOne)
	_, server, dataDir := newTestModule(t, connect.URL)

	body, contentType := prusaSlicerUpload(t, true, "jigs", "spacer.gcode", []byte("G28\n"))

	status, answer := send(t, http.MethodPost, server.URL+"/core-one/api/files/local", goodToken, contentType, body)
	if status != http.StatusCreated {
		t.Fatalf("status %d: %s", status, answer)
	}

	if len(fake.prints) != 1 {
		t.Fatalf("prints %+v", fake.prints)
	}

	want := printRequest{Storage: "local", Source: "slicer-uploads/jigs/spacer.gcode", Path: "usb/jigs/spacer.gcode", Replace: true}
	if fake.prints[0] != want || fake.printDevice[0] != coreOne.ID {
		t.Fatalf("print %+v on %s, want %+v on %s", fake.prints[0], fake.printDevice[0], want, coreOne.ID)
	}

	assertNoLeftovers(t, dataDir)
}

func TestRefusalsReachTheSlicer(t *testing.T) {
	fake, connect := newFakeConnect(t, coreOne)
	_, server, dataDir := newTestModule(t, connect.URL)

	cases := []struct {
		label    string
		folder   string
		filename string
		content  []byte
		status   int
		contains string
	}{
		{"not g-code", "", "model.stl", []byte("solid"), http.StatusUnsupportedMediaType, "not a file a printer prints"},
		{"empty file", "", "empty.gcode", nil, http.StatusBadRequest, "is empty"},
		{"climbing path", "../../etc", "x.gcode", []byte("G28"), http.StatusBadRequest, "inside the storage"},
	}

	for _, test := range cases {
		body, contentType := prusaSlicerUpload(t, false, test.folder, test.filename, test.content)

		status, answer := send(t, http.MethodPost, server.URL+"/api/files/local", goodToken, contentType, body)
		if status != test.status || !strings.Contains(answer, test.contains) {
			t.Errorf("%s: %d %s", test.label, status, answer)
		}
	}

	status, answer := send(t, http.MethodPost, server.URL+"/api/files/local", goodToken, "text/plain", strings.NewReader("hello"))
	if status != http.StatusBadRequest || !strings.Contains(answer, "multipart") {
		t.Errorf("not multipart: %d %s", status, answer)
	}

	fake.refuseStart = true
	body, contentType := prusaSlicerUpload(t, true, "", "busy.gcode", []byte("G28"))

	status, answer = send(t, http.MethodPost, server.URL+"/api/files/local", goodToken, contentType, body)
	if status != http.StatusConflict || !strings.Contains(answer, "a print is already running") {
		t.Errorf("printer refused: %d %s", status, answer)
	}

	fake.refuseStart = false
	fake.status = http.StatusForbidden
	body, contentType = prusaSlicerUpload(t, true, "", "denied.gcode", []byte("G28"))

	status, answer = send(t, http.MethodPost, server.URL+"/api/files/local", goodToken, contentType, body)
	if status != http.StatusForbidden || !strings.Contains(answer, "needs permission") {
		t.Errorf("credential without permission: %d %s", status, answer)
	}

	assertNoLeftovers(t, dataDir)
}

// TestLargeUploadStreams sends 200 MiB through the module and checks every
// byte arrives, with the multipart body generated on the fly so the test holds
// no copy of it.
func TestLargeUploadStreams(t *testing.T) {
	if testing.Short() {
		t.Skip("200 MiB upload")
	}

	fake, connect := newFakeConnect(t, coreOne)
	_, server, dataDir := newTestModule(t, connect.URL)

	const size = 200 << 20

	reader, writer := io.Pipe()
	form := multipart.NewWriter(writer)
	hash := sha256.New()

	go func() {
		_ = form.WriteField("print", "false")
		_ = form.WriteField("path", "")
		part, _ := form.CreateFormFile("file", "big.bgcode")

		chunk := bytes.Repeat([]byte("G1 X10 Y10 E0.5\n"), 4096)
		written := 0

		for written < size {
			piece := chunk
			if size-written < len(piece) {
				piece = piece[:size-written]
			}

			_, _ = part.Write(piece)
			_, _ = hash.Write(piece)
			written += len(piece)
		}

		_ = form.Close()
		_ = writer.Close()
	}()

	status, answer := send(t, http.MethodPost, server.URL+"/api/files/local", goodToken, form.FormDataContentType(), reader)
	if status != http.StatusCreated {
		t.Fatalf("status %d: %s", status, answer)
	}

	key := "local/slicer-uploads/big.bgcode"
	if len(fake.stored[key]) != size || fake.storedHash[key] != hex.EncodeToString(hash.Sum(nil)) {
		t.Fatalf("stored %d bytes, digest %s", len(fake.stored[key]), fake.storedHash[key])
	}

	assertNoLeftovers(t, dataDir)
}

func TestConcurrentUploads(t *testing.T) {
	fake, connect := newFakeConnect(t, coreOne)
	_, server, dataDir := newTestModule(t, connect.URL)

	var wait sync.WaitGroup

	for index := range 8 {
		wait.Add(1)

		go func() {
			defer wait.Done()

			name := "part" + string(rune('a'+index)) + ".gcode"
			body, contentType := prusaSlicerUpload(t, false, "", name, bytes.Repeat([]byte{byte('a' + index)}, 1<<20))

			if status, answer := send(t, http.MethodPost, server.URL+"/api/files/local", goodToken, contentType, body); status != http.StatusCreated {
				t.Errorf("%s: %d %s", name, status, answer)
			}
		}()
	}

	wait.Wait()

	for index := range 8 {
		name := "local/slicer-uploads/part" + string(rune('a'+index)) + ".gcode"
		if got := fake.stored[name]; len(got) != 1<<20 || got[0] != byte('a'+index) {
			t.Errorf("%s holds %d bytes", name, len(got))
		}
	}

	assertNoLeftovers(t, dataDir)
}

func TestFileNamesWithSpacesAndAccents(t *testing.T) {
	fake, connect := newFakeConnect(t, coreOne)
	_, server, _ := newTestModule(t, connect.URL)

	name := "Držák kabelu v2 (final).bgcode"
	body, contentType := prusaSlicerUpload(t, true, "", name, []byte("G28"))

	if status, answer := send(t, http.MethodPost, server.URL+"/api/files/local", goodToken, contentType, body); status != http.StatusCreated {
		t.Fatalf("status %d: %s", status, answer)
	}

	if _, ok := fake.stored["local/slicer-uploads/"+name]; !ok || fake.prints[0].Path != "usb/"+name {
		t.Fatalf("stored %v, printed %+v", keys(fake.stored), fake.prints)
	}
}

func TestSettingsDefaults(t *testing.T) {
	got := Settings{Folder: "/uploads/"}.withDefaults()
	want := Settings{ConnectURL: "http://127.0.0.1:8080", Storage: "local", Folder: "uploads", PrinterStorage: "usb"}

	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}

	if _, err := newConnectClient("ftp://example"); err == nil {
		t.Fatal("an ftp connect_url was accepted")
	}
}

func assertNoLeftovers(t *testing.T, dataDir string) {
	t.Helper()

	leftovers, _ := filepath.Glob(filepath.Join(dataDir, incomingFolder, "*"))
	if len(leftovers) != 0 {
		t.Fatalf("uploads left behind in the data directory: %v", leftovers)
	}

	if _, err := os.Stat(dataDir); err != nil {
		t.Fatal(err)
	}
}

func keys(stored map[string][]byte) []string {
	names := make([]string, 0, len(stored))
	for name := range stored {
		names = append(names, name)
	}

	return names
}
