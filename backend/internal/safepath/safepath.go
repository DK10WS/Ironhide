// Package safepath validates untrusted path-like input before it is used as
// an object key or a filesystem path.
//
// Every function here is written for attacker-controlled input. The rule
// throughout is to reject rather than sanitise: quietly rewriting a hostile
// path into a harmless one hides the attempt and produces confusing keys,
// whereas an error tells the caller something happened.
package safepath

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

// ErrUnsafe is returned for any input that could escape its intended
// directory or confuse a downstream consumer.
var ErrUnsafe = errors.New("unsafe path")

// Relative validates name and returns it as a clean slash-separated relative
// path suitable for joining onto a fixed prefix.
//
// Rejected:
//   - empty, ".", ".."
//   - absolute paths ("/etc/passwd")
//   - Windows drive letters ("C:/windows")
//   - any ".." segment, anywhere
//   - backslashes, which some archivers emit and which mean path separators
//     on Windows but not on Linux
//   - control characters, including NUL, which truncate strings in C callers
//     and break header parsing
//
// Accepted: "index.html", "assets/app.css", "deep/nested/file.js"
//
// The result is always cleaned, so "a//b/./c" and "a/b/c" collapse to the same
// key rather than producing two objects.
func Relative(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("%w: empty path", ErrUnsafe)
	}

	// NUL and other controls are rejected before any other inspection.
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("%w: path contains a control character", ErrUnsafe)
		}
	}

	// A backslash is a separator on some platforms and a literal on others;
	// treating it as literal is how "..\\..\\x" style entries slip through.
	if strings.Contains(name, `\`) {
		return "", fmt.Errorf("%w: path contains a backslash", ErrUnsafe)
	}

	if strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("%w: path is absolute", ErrUnsafe)
	}

	// Drive letter, e.g. "C:foo" or "C:/foo".
	if len(name) >= 2 && name[1] == ':' {
		return "", fmt.Errorf("%w: path contains a drive letter", ErrUnsafe)
	}

	// Reject traversal on the raw segments rather than relying on path.Clean.
	// Clean("/"+name) would turn "../../etc/passwd" into "/etc/passwd", which
	// silently accepts the attempt instead of reporting it.
	for _, seg := range strings.Split(name, "/") {
		if seg == ".." {
			return "", fmt.Errorf("%w: path contains a parent reference", ErrUnsafe)
		}
	}

	clean := path.Clean(name)
	switch {
	case clean == "." || clean == ".." || clean == "/":
		return "", fmt.Errorf("%w: path resolves to nothing", ErrUnsafe)
	case strings.HasPrefix(clean, "../"):
		return "", fmt.Errorf("%w: path escapes its directory", ErrUnsafe)
	case strings.HasPrefix(clean, "/"):
		return "", fmt.Errorf("%w: path is absolute", ErrUnsafe)
	}

	// A trailing slash means a directory. Callers storing files should not
	// silently drop it and end up with a key that looks like a directory.
	clean = strings.TrimSuffix(clean, "/")
	if clean == "" {
		return "", fmt.Errorf("%w: path resolves to a directory", ErrUnsafe)
	}

	if len(clean) > 1024 {
		return "", fmt.Errorf("%w: path is too long (%d bytes)", ErrUnsafe, len(clean))
	}

	return clean, nil
}

// Join validates rel and returns it appended to prefix. The prefix is trusted
// and is not re-validated, which is the point: it comes from the server.
func Join(prefix, rel string) (string, error) {
	clean, err := Relative(rel)
	if err != nil {
		return "", err
	}
	return prefix + clean, nil
}

// ValidateKey checks a complete object key.
//
// It applies the same rules as Relative but permits the leading prefix
// segments a full key necessarily has. Use this when checking a key that has
// already been assembled, and Relative when checking a name that still needs
// to be joined onto a prefix.
func ValidateKey(key string) error {
	if key == "" {
		return fmt.Errorf("%w: empty key", ErrUnsafe)
	}
	for _, r := range key {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: key contains a control character", ErrUnsafe)
		}
	}
	if strings.Contains(key, `\`) {
		return fmt.Errorf("%w: key contains a backslash", ErrUnsafe)
	}
	if strings.HasPrefix(key, "/") {
		return fmt.Errorf("%w: key is absolute", ErrUnsafe)
	}
	if len(key) >= 2 && key[1] == ':' {
		return fmt.Errorf("%w: key contains a drive letter", ErrUnsafe)
	}
	for _, seg := range strings.Split(key, "/") {
		switch seg {
		case "..":
			return fmt.Errorf("%w: key contains a parent reference", ErrUnsafe)
		case ".":
			return fmt.Errorf("%w: key contains a current-directory reference", ErrUnsafe)
		}
	}
	if strings.Contains(key, "//") {
		return fmt.Errorf("%w: key contains an empty segment", ErrUnsafe)
	}
	if len(key) > 1024 {
		return fmt.Errorf("%w: key is too long (%d bytes)", ErrUnsafe, len(key))
	}
	return nil
}
