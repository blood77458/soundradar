//go:build !windows

package hotkey

import "errors"

// ErrUnsupported is returned on every platform without a Win32 message queue.
var ErrUnsupported = errors.New("全局热键只支持 Windows（RegisterHotKey 是 Win32 API）")

// NewManager always fails off Windows. The type and its methods still exist so
// the CLI compiles everywhere and can report the limitation in the banner
// instead of failing to build.
func NewManager() (*Manager, error) { return nil, ErrUnsupported }

// wake is a no-op off Windows; there is no message queue to interrupt.
func (m *Manager) wake() {}
