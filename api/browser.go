package api

import "github.com/dop251/goja"

// Browser is the public interface of a CDP browser.
type Browser interface {
	Close()
	Context() BrowserContextAPI
	IsConnected() bool
	NewContext(opts goja.Value) (BrowserContextAPI, error)
	NewPage(opts goja.Value) (Page, error)
	On(string) (bool, error)
	UserAgent() string
	Version() string
}
