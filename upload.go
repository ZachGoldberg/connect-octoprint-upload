package main

import (
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
	token, ok := credential(w, r)
	if !ok {
		return
	}

	_, connect, _ := m.current()

	found, err := findPrinter(r, connect, token, r.PathValue("printer"))
	if err != nil {
		m.fail(w, r, err)

		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"api":    "0.1",
		"server": octoPrintVersion,
		"text":   fmt.Sprintf("OctoPrint %s (Prusa Connect %s %s for %s)", octoPrintVersion, name, release, printerLabel(found)),
	})
}

// upload is OctoPrint's POST /api/files/local: a multipart form with "print",
// "path" and the file (PrusaSlicer 2.9 sends them in that order). The file is
// kept in the server's storage either way; with print it is also sent to the
// printer, replacing a file of that name there, and started.
func (m *module) upload(w http.ResponseWriter, r *http.Request) {
	token, ok := credential(w, r)
	if !ok {
		return
	}

	settings, connect, dataDir := m.current()

	found, err := findPrinter(r, connect, token, r.PathValue("printer"))
	if err != nil {
		m.fail(w, r, err)

		return
	}

	received, err := receiveForm(r, filepath.Join(dataDir, incomingFolder))
	if received.held != "" {
		defer os.Remove(received.held)
	}

	if err != nil {
		m.fail(w, r, err)

		return
	}

	held, err := os.Open(received.held)
	if err != nil {
		m.fail(w, r, err)

		return
	}
	defer held.Close()

	relative := path.Join(received.folder, received.filename)

	saved, err := connect.store(r.Context(), token, settings.Storage, path.Join(settings.Folder, relative), held, received.size)
	if err != nil {
		m.fail(w, r, fmt.Errorf("store the file in Prusa Connect: %w", err))

		return
	}

	logged := []any{"printer", printerLabel(found), "file", saved.Path, "bytes", saved.Bytes, "print", received.print}

	if received.print {
		started, printErr := connect.print(r.Context(), token, found.ID, printRequest{
			Storage: settings.Storage,
			Source:  saved.Path,
			Path:    path.Join(settings.PrinterStorage, relative),
			Replace: true,
		})
		if printErr != nil {
			m.fail(w, r, fmt.Errorf("start the print: %w", printErr))

			return
		}

		logged = append(logged, "printer_path", started.Path)
	}

	m.log.Info("uploaded", logged...)

	writeJSON(w, http.StatusCreated, map[string]any{
		"done": true,
		"files": map[string]any{
			"local": map[string]any{
				"name":   received.filename,
				"path":   relative,
				"origin": "local",
				"refs":   map[string]string{"resource": saved.Path},
			},
		},
	})
}

// credential reads the slicer's API key, which is a Prusa Connect API
// credential: the module acts as the person who created it.
func credential(w http.ResponseWriter, r *http.Request) (string, bool) {
	token := strings.TrimSpace(r.Header.Get("X-Api-Key"))
	if token == "" {
		http.Error(w, "missing API key: create an API credential in Prusa Connect (Settings > API credentials) "+
			"and put it in the slicer's API key field", http.StatusUnauthorized)

		return "", false
	}

	return token, true
}

// findPrinter resolves the path segment to a printer the person may see: its
// id, its name compared without case, spaces or punctuation, or a unique
// prefix of that name. With no segment it takes the only printer, if there is
// exactly one.
func findPrinter(r *http.Request, connect *connectClient, token, wanted string) (device, error) {
	if connect == nil {
		return device{}, &httpError{http.StatusServiceUnavailable, "the module has not been configured yet"}
	}

	printers, err := connect.devices(r.Context(), token)
	if err != nil {
		return device{}, err
	}

	if len(printers) == 0 {
		return device{}, &httpError{http.StatusNotFound, "this API credential sees no printers in Prusa Connect"}
	}

	if wanted == "" {
		if len(printers) == 1 {
			return printers[0], nil
		}

		return device{}, &httpError{http.StatusNotFound,
			"there are " + fmt.Sprint(len(printers)) + " printers; put one in the host address, e.g. /" +
				slug(printerLabel(printers[0])) + " (printers: " + strings.Join(namesOf(printers), ", ") + ")"}
	}

	for _, candidate := range printers {
		if candidate.ID == wanted || slug(candidate.Name) == slug(wanted) {
			return candidate, nil
		}
	}

	// Connect names a printer after its model and serial ("COREONE <serial>"),
	// so a unique prefix ("core-one") also names it.
	var prefixed []device

	for _, candidate := range printers {
		if slug(wanted) != "" && strings.HasPrefix(slug(candidate.Name), slug(wanted)) {
			prefixed = append(prefixed, candidate)
		}
	}

	if len(prefixed) == 1 {
		return prefixed[0], nil
	}

	return device{}, &httpError{http.StatusNotFound,
		"no printer named " + strconv.Quote(wanted) + " (printers: " + strings.Join(namesOf(printers), ", ") + ")"}
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

	var connectRefusal *connectError

	switch {
	case errors.As(err, &refusal):
		status = refusal.status
	case errors.As(err, &connectRefusal):
		status = connectRefusal.Status

		switch connectRefusal.Status {
		case http.StatusUnauthorized:
			err = fmt.Errorf("Prusa Connect does not accept this API key: use an API credential from Settings > API credentials (%w)", err)
		case http.StatusForbidden:
			err = fmt.Errorf("Prusa Connect refused this API credential: it needs permission to upload files and operate this printer (%w)", err)
		}
	}

	m.log.Warn("request refused", "path", r.URL.Path, "status", status, "err", err)
	http.Error(w, err.Error(), status)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func printerLabel(printer device) string {
	if printer.Name != "" {
		return printer.Name
	}

	return printer.ID
}

func namesOf(printers []device) []string {
	names := make([]string, 0, len(printers))
	for _, printer := range printers {
		names = append(names, printerLabel(printer))
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
