// Command octoprint-upload is a Prusa Connect service module. It lets a slicer
// that speaks OctoPrint's upload API (PrusaSlicer 2.x's "OctoPrint" host type)
// send G-code into a self-hosted Prusa Connect, and optionally print it.
//
// The slicer talks to a port of the module's own, granted by the operator at
// install (see module.yaml for why it is not a route under the host's API). An
// upload is held in the module's data directory until it has fully arrived,
// written into a storage of the server with storage.write, and, when the
// slicer asks, printed with device.print, which has the server send the file
// to the printer and start it.
package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/prusa3d/prusa-connect/sdk/deviceapi"
	"github.com/prusa3d/prusa-connect/sdk/extension"
	"github.com/prusa3d/prusa-connect/sdk/routesapi"
	"github.com/prusa3d/prusa-connect/sdk/storageapi"
	"github.com/prusa3d/prusa-connect/sdk/wire"
	"github.com/prusa3d/prusa-connect/sdk/wirerpc"
)

//go:embed module.yaml
var manifestYAML []byte

//go:embed openapi.json
var openAPI []byte

var (
	name    = manifestScalar(manifestYAML, "name")
	release = extension.Version(manifestScalar(manifestYAML, "version"))
)

// listenName is the port's name in module.yaml's needs block.
const listenName = "octoprint"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	line, err := wire.Handshake{
		Protocol: wire.ProtocolMajor,
		MinMinor: wire.MinMinor,
		MaxMinor: wire.MaxMinor,
		Address:  wire.StdioAddress,
	}.Line()
	if err != nil {
		return fmt.Errorf("render the handshake: %w", err)
	}

	if _, err = os.Stdout.WriteString(line); err != nil {
		return fmt.Errorf("write the handshake: %w", err)
	}

	serves := wirerpc.Methods{}

	conn, err := wirerpc.NewConn(stdio{}, wirerpc.Options{Role: wirerpc.RoleChild, Methods: serves})
	if err != nil {
		return fmt.Errorf("build the session: %w", err)
	}

	held := &module{
		devices: deviceapi.NewClient(conn),
		files:   storageapi.NewClient(conn),
		log:     slog.New(slog.NewTextHandler(os.Stderr, nil)).With("module", name),
	}
	defer held.stop()

	maps.Copy(serves, held.methods())

	fmt.Fprintf(os.Stderr, "%s %s is serving on stdio as pid %d\n", name, release, os.Getpid())

	if err = conn.Serve(ctx); err != nil {
		return fmt.Errorf("serve: %w", err)
	}

	return nil
}

// Devices is the part of the host's fleet API this module calls.
type Devices interface {
	List(ctx context.Context, params deviceapi.ListParams) (deviceapi.Page, error)
	Print(ctx context.Context, params deviceapi.PrintParams) (deviceapi.Started, error)
}

// Files is the part of the host's storage API this module calls.
type Files interface {
	Write(ctx context.Context, params storageapi.WriteParams, body io.Reader) (storageapi.Written, error)
}

// module is everything the process holds once the host sent its settings.
type module struct {
	devices Devices
	files   Files
	log     *slog.Logger

	mu       sync.Mutex
	settings Settings
	dataDir  string
	port     int
	problem  string
	server   *http.Server
}

func (m *module) methods() wirerpc.Methods {
	return wirerpc.Methods{
		wirerpc.MethodInfo: func(context.Context, wirerpc.Call) (any, error) {
			return routesapi.Describe(wirerpc.Info{
				Name:     name,
				Version:  release,
				Contract: wirerpc.Contract,
				Serves:   []wirerpc.Method{wirerpc.MethodConfigure},
			}, openAPI)
		},
		wirerpc.MethodShutdown: func(context.Context, wirerpc.Call) (any, error) {
			fmt.Fprintf(os.Stderr, "%s was asked to stop\n", name)
			m.stop()

			return nil, nil
		},
		wirerpc.MethodConfigure: wirerpc.ConfigureGranted(m.configure),
		routesapi.MethodServe:   routesapi.Serve(m.hostRoutes()),
	}
}

// configure takes the settings and starts the upload listener on the granted
// port. A missing grant is not an error: the module stays up and says what is
// missing on GET /status, so the operator reads the reason.
func (m *module) configure(_ context.Context, sent Settings, grants wirerpc.Grants) error {
	sent = sent.withDefaults()

	m.mu.Lock()
	defer m.mu.Unlock()

	m.settings = sent

	if m.server != nil {
		return nil
	}

	grant, granted := grants.Listen[listenName]

	switch {
	case !granted:
		m.problem = "the operator granted no port; install again with -approve-listen"

		return nil
	case grant.Network != "tcp":
		m.problem = "the granted port is " + grant.Network + ", and the upload API needs tcp"

		return nil
	case grants.Data == "":
		m.problem = "the operator granted no data directory; install again with -approve"

		return nil
	}

	m.dataDir = grants.Data
	m.port = grant.Port
	clearIncoming(m.dataDir, m.log)

	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", grant.Port))
	if err != nil {
		m.problem = "cannot listen on tcp " + fmt.Sprint(grant.Port) + ": " + err.Error()

		return nil
	}

	m.server = &http.Server{
		Handler:           m.uploadRoutes(),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	go func() {
		if serveErr := m.server.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			m.log.Error("the upload listener stopped", "err", serveErr)
		}
	}()

	m.problem = ""
	m.log.Info("serving the OctoPrint upload API", "port", grant.Port, "storage", sent.Storage, "folder", sent.Folder)

	return nil
}

func (m *module) stop() {
	m.mu.Lock()
	server := m.server
	m.server = nil
	m.mu.Unlock()

	if server == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_ = server.Shutdown(ctx)
}

// current returns the settings, the data directory and whether the listener
// is up, under the lock.
func (m *module) current() (Settings, string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.settings, m.dataDir
}

// hostRoutes are the routes under /api/v1/modules/<namespace>/, authorized by
// the host. They only report state; uploads go to the module's own port.
func (m *module) hostRoutes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()

		writeJSON(w, http.StatusOK, map[string]any{
			"serving":     m.server != nil,
			"port":        m.port,
			"problem":     m.problem,
			"key_set":     m.settings.APIKey != "",
			"storage":     m.settings.Storage,
			"folder":      m.settings.Folder,
			"version":     release,
			"slicer_host": "http://<this server>:" + fmt.Sprint(m.port) + "/<printer name>",
		})
	})

	return mux
}

// manifestScalar reads one top-level scalar out of module.yaml, so the name
// and version have one source.
func manifestScalar(document []byte, key string) string {
	for _, line := range strings.Split(string(document), "\n") {
		value, found := strings.CutPrefix(line, key+":")
		if !found {
			continue
		}

		if comment := strings.Index(value, " #"); comment >= 0 {
			value = value[:comment]
		}

		return strings.Trim(strings.TrimSpace(value), `"'`)
	}

	return ""
}

// stdio is the channel the host handed this process.
type stdio struct{}

func (stdio) Read(p []byte) (int, error)  { return os.Stdin.Read(p) }
func (stdio) Write(p []byte) (int, error) { return os.Stdout.Write(p) }
func (stdio) Close() error                { return os.Stdin.Close() }
