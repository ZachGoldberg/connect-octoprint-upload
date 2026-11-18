package main

import "strings"

// Settings is the module's options from module.yaml, as the host sends them
// on plugin.configure.
type Settings struct {
	APIKey         string `json:"api_key"`
	Storage        string `json:"storage"`
	Folder         string `json:"folder"`
	PrinterStorage string `json:"printer_storage"`
}

// withDefaults fills what the operator left empty, and trims slashes so the
// values join into paths.
func (s Settings) withDefaults() Settings {
	s.APIKey = strings.TrimSpace(s.APIKey)

	if s.Storage = strings.TrimSpace(s.Storage); s.Storage == "" {
		s.Storage = "local"
	}

	s.Folder = strings.Trim(strings.TrimSpace(s.Folder), "/")

	if s.PrinterStorage = strings.Trim(strings.TrimSpace(s.PrinterStorage), "/"); s.PrinterStorage == "" {
		s.PrinterStorage = "usb"
	}

	return s
}
