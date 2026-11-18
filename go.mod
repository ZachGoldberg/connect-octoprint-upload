module github.com/zachgoldberg/connect-octoprint-upload

go 1.26.11

require github.com/prusa3d/prusa-connect/sdk v0.0.0

// The SDK is not published as a Go module yet, so it is built from a
// Prusa Connect checkout in .core/ (a symlink or clone of
// github.com/prusa3d/prusa-connect at the release this targets).
replace github.com/prusa3d/prusa-connect/sdk => ./.core/sdk
