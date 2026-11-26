// Package crashpoint marks places where recovery tests kill the process.
// Hook is nil in the server, so Hit does nothing there.
package crashpoint

// Hook is set only by tests, before any storage operation starts.
var Hook func(name string)

// Hit reports that execution reached the named point.
func Hit(name string) {
	if Hook != nil {
		Hook(name)
	}
}
