package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/prusa3d/prusa-connect/sdk/deviceapi"
	"github.com/prusa3d/prusa-connect/sdk/storageapi"
)

// octoPrintVersion is the OctoPrint version the API answers as. PrusaSlicer
// only checks that the "text" of /api/version starts with "OctoPrint".
const octoPrintVersion = "1.10.0"

// incomingFolder holds uploads while they arrive, inside the data directory.
const incomingFolder = "incoming"

// uploadRoutes is the OctoPrint-compatible API on the module's own port. The
// printer is the first path segment, so one slicer profile per printer:
// http://<server>:17481/<printer name or id>. With exactly one printer in the
// fleet, the segment may be left out.
func (m *module) uploadRoutes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/version", m.version)
	mux.HandleFunc("GET /{printer}/api/version", m.version)
	mux.HandleFunc("POST /api/files/local", m.upload)
	mux.HandleFunc("POST /{printer}/api/files/local", m.upload)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })

	return mux
}

func (m *module) version(w http.ResponseWriter, r *http.Request) {
	if !m.authorized(w, r) {
		return
	}

	device, err := m.findPrinter(r, r.PathValue("printer"))
	if err != nil {
		m.fail(w, r, err)

		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"api":    "0.1",
		"server": octoPrintVersion,
		"text":   fmt.Sprintf("OctoPrint %s (Prusa Connect %s %s for %s)", octoPrintVersion, name, release, printerLabel(device)),
	})
}

// upload is OctoPrint's POST /api/files/local: a multipart form with the file
// and, optionally, "print" (start it) and "path" (a folder). The file is kept
// in the server's storage either way; with print it is also sent to the
// printer and started.
func (m *module) upload(w http.ResponseWriter, r *http.Request) {
	if !m.authorized(w, r) {
		return
	}

	settings, dataDir := m.current()

	device, err := m.findPrinter(r, r.PathValue("printer"))
	if err != nil {
		m.fail(w, r, err)

		return
	}

	form, err := receiveForm(r, filepath.Join(dataDir, incomingFolder))
	if form.held != "" {
		defer os.Remove(form.held)
	}

	if err != nil {
		m.fail(w, r, err)

		return
	}

	stored := path.Join(settings.Folder, form.folder, form.filename)

	held, err := os.Open(form.held)
	if err != nil {
		m.fail(w, r, err)

		return
	}
	defer held.Close()

	written, err := m.files.Write(r.Context(), storageapi.WriteParams{
		Storage: settings.Storage,
		Path:    stored,
		Size:    form.size,
	}, held)
	if err != nil {
		m.fail(w, r, fmt.Errorf("store %s in storage %q: %w", stored, settings.Storage, err))

		return
	}

	logged := []any{"printer", printerLabel(device), "file", written.Path, "bytes", written.Size, "print", form.print}

	if form.print {
		onPrinter, started, printErr := m.print(r, device.ID, settings, written.Path, path.Join(form.folder, form.filename))
		if printErr != nil {
			m.fail(w, r, printErr)

			return
		}

		logged = append(logged, "printer_path", onPrinter, "transfer", started.TransferID)
	}

	m.log.Info("uploaded", logged...)

	writeJSON(w, http.StatusCreated, map[string]any{
		"done": true,
		"files": map[string]any{
			"local": map[string]any{
				"name":   form.filename,
				"path":   path.Join(form.folder, form.filename),
				"origin": "local",
				"refs":   map[string]string{"resource": written.Path},
			},
		},
	})
}

// print has the server send the stored file to the printer and start it. The
// SDK has no "replace", so when the printer already holds a file of that name
// it retries once under a name with the time in it.
func (m *module) print(r *http.Request, deviceID string, settings Settings, source, wanted string) (string, deviceapi.Started, error) {
	onPrinter := path.Join(settings.PrinterStorage, wanted)

	params := deviceapi.PrintParams{ID: deviceID, Storage: settings.Storage, Source: source, Path: onPrinter}

	started, err := m.devices.Print(r.Context(), params)
	if errors.Is(err, deviceapi.ErrNameTaken) {
		extension := path.Ext(wanted)
		params.Path = path.Join(settings.PrinterStorage,
			strings.TrimSuffix(wanted, extension)+time.Now().Format("_150405")+extension)
		started, err = m.devices.Print(r.Context(), params)
	}

	if err != nil {
		return "", deviceapi.Started{}, fmt.Errorf("start the print: %w", err)
	}

	return params.Path, started, nil
}

// authorized checks the slicer's X-Api-Key against the module's api_key.
func (m *module) authorized(w http.ResponseWriter, r *http.Request) bool {
	settings, _ := m.current()

	if settings.APIKey == "" {
		http.Error(w, "this module has no api_key set yet: set it on the module's settings in Prusa Connect", http.StatusServiceUnavailable)

		return false
	}

	sent := strings.TrimSpace(r.Header.Get("X-Api-Key"))
	if sent == "" || subtle.ConstantTimeCompare([]byte(sent), []byte(settings.APIKey)) != 1 {
		http.Error(w, "wrong or missing API key", http.StatusForbidden)

		return false
	}

	return true
}

// findPrinter resolves the path segment to a printer: its id, or its name
// compared without case, spaces or punctuation. With no segment it takes the
// only printer, if there is exactly one.
func (m *module) findPrinter(r *http.Request, wanted string) (deviceapi.Device, error) {
	page, err := m.devices.List(r.Context(), deviceapi.ListParams{Limit: 100})
	if err != nil {
		return deviceapi.Device{}, fmt.Errorf("list the printers: %w", err)
	}

	if wanted == "" {
		if len(page.Items) == 1 {
			return page.Items[0], nil
		}

		return deviceapi.Device{}, &httpError{http.StatusNotFound,
			"there are " + fmt.Sprint(len(page.Items)) + " printers; put one in the host address, e.g. /" + slug(namesOf(page.Items)[0]) + " (printers: " + strings.Join(namesOf(page.Items), ", ") + ")"}
	}

	for _, device := range page.Items {
		if device.ID == wanted || slug(device.Name) == slug(wanted) {
			return device, nil
		}
	}

	// A unique prefix also names a printer: Connect names a printer after its
	// model and serial ("COREONE <serial>"), and "core-one" should find it.
	var prefixed []deviceapi.Device

	for _, device := range page.Items {
		if slug(wanted) != "" && strings.HasPrefix(slug(device.Name), slug(wanted)) {
			prefixed = append(prefixed, device)
		}
	}

	if len(prefixed) == 1 {
		return prefixed[0], nil
	}

	return deviceapi.Device{}, &httpError{http.StatusNotFound,
		"no printer named " + strconv.Quote(wanted) + " (printers: " + strings.Join(namesOf(page.Items), ", ") + ")"}
}

// form is one OctoPrint upload as received.
type form struct {
	held     string
	size     int64
	filename string
	folder   string
	print    bool
}

// receiveForm reads the multipart body, copying the file part into a file
// under incoming, because storage.write needs the size before the bytes.
func receiveForm(r *http.Request, incoming string) (form, error) {
	var received form

	reader, err := r.MultipartReader()
	if err != nil {
		return received, &httpError{http.StatusBadRequest, "expected a multipart/form-data upload"}
	}

	for {
		part, nextErr := reader.NextPart()
		if errors.Is(nextErr, io.EOF) {
			break
		}

		if nextErr != nil {
			return received, &httpError{http.StatusBadRequest, "malformed multipart body: " + nextErr.Error()}
		}

		switch part.FormName() {
		case "file":
			err = holdFile(part, incoming, &received)
		case "print":
			received.print, err = readBool(part)
		case "path":
			received.folder, err = readFolder(part)
		}

		part.Close()

		if err != nil {
			return received, err
		}
	}

	if received.held == "" {
		return received, &httpError{http.StatusBadRequest, "the upload carries no file part"}
	}

	return received, nil
}

func holdFile(part *multipart.Part, incoming string, received *form) error {
	filename := path.Base(strings.ReplaceAll(part.FileName(), `\`, "/"))
	if filename == "" || filename == "." || filename == "/" || filename == ".." {
		return &httpError{http.StatusBadRequest, "the file part has no usable file name"}
	}

	if !storageapi.IsPrintableExtension(filename) {
		return &httpError{http.StatusUnsupportedMediaType, filename + " is not a file a printer prints (.gcode or .bgcode)"}
	}

	if err := os.MkdirAll(incoming, 0o700); err != nil {
		return err
	}

	held, err := os.CreateTemp(incoming, "upload-*")
	if err != nil {
		return err
	}
	defer held.Close()

	received.held = held.Name()
	received.filename = filename

	size, err := io.Copy(held, io.LimitReader(part, storageapi.MaxWriteBytes+1))
	if err != nil {
		return fmt.Errorf("receive %s: %w", filename, err)
	}

	switch {
	case size == 0:
		return &httpError{http.StatusBadRequest, filename + " is empty"}
	case size > storageapi.MaxWriteBytes:
		return &httpError{http.StatusRequestEntityTooLarge, filename + " is larger than the 4 GiB a storage takes"}
	}

	received.size = size

	return nil
}

func readBool(part *multipart.Part) (bool, error) {
	raw, err := io.ReadAll(io.LimitReader(part, 16))
	if err != nil {
		return false, err
	}

	value, err := strconv.ParseBool(strings.TrimSpace(string(raw)))
	if err != nil {
		return false, &httpError{http.StatusBadRequest, "print must be true or false"}
	}

	return value, nil
}

func readFolder(part *multipart.Part) (string, error) {
	raw, err := io.ReadAll(io.LimitReader(part, 1024))
	if err != nil {
		return "", err
	}

	folder := strings.Trim(strings.ReplaceAll(strings.TrimSpace(string(raw)), `\`, "/"), "/")
	if folder == "" {
		return "", nil
	}

	clean := path.Clean(folder)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", &httpError{http.StatusBadRequest, "path must stay inside the storage"}
	}

	return clean, nil
}

// clearIncoming removes uploads a previous run left half-received.
func clearIncoming(dataDir string, log *slog.Logger) {
	leftovers, _ := filepath.Glob(filepath.Join(dataDir, incomingFolder, "upload-*"))
	for _, leftover := range leftovers {
		if err := os.Remove(leftover); err == nil {
			log.Info("removed an unfinished upload", "file", filepath.Base(leftover))
		}
	}
}

// httpError is a refusal with the status the slicer should see.
type httpError struct {
	status  int
	message string
}

func (e *httpError) Error() string { return e.message }

// fail answers an error with a status the slicer shows usefully: PrusaSlicer
// prints the body of a failed upload.
func (m *module) fail(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusBadGateway

	var refusal *httpError

	switch {
	case errors.As(err, &refusal):
		status = refusal.status
	case errors.Is(err, deviceapi.ErrDenied):
		status = http.StatusForbidden
		err = fmt.Errorf("%w (grant this module operate on the printer, and read_storage/write_storage on the storage)", err)
	case errors.Is(err, deviceapi.ErrNoFile):
		status = http.StatusNotFound
	}

	m.log.Warn("request refused", "path", r.URL.Path, "status", status, "err", err)
	http.Error(w, err.Error(), status)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func printerLabel(device deviceapi.Device) string {
	if device.Name != "" {
		return device.Name
	}

	return device.ID
}

func namesOf(devices []deviceapi.Device) []string {
	names := make([]string, 0, len(devices))
	for _, device := range devices {
		names = append(names, printerLabel(device))
	}

	if len(names) == 0 {
		names = append(names, "none")
	}

	return names
}

// slug compares printer names loosely: "CORE One", "core-one" and "coreone"
// are one name.
func slug(text string) string {
	var kept strings.Builder

	for _, character := range strings.ToLower(text) {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') {
			kept.WriteRune(character)
		}
	}

	return kept.String()
}
