// Package config reads each mode's configuration from the environment.
// Secrets come from files (a mounted Secret), never from a variable's
// value in a manifest: GORDON_DATABASE_URL_FILE, the key and token files.
// Every problem is reported at once, and a mode refuses to start on any.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/sperano/gordon/internal/logging"
)

// Getenv looks a variable up; os.Getenv in production.
type Getenv func(string) string

// Common is what every mode reads.
type Common struct {
	DatabaseURL string
	AdminAddr   string
	LogLevel    slog.Level
}

// Defaults shared by the modes.
const (
	DefaultAdminAddr     = ":9090"
	DefaultListenAddr    = ":8080"
	DefaultShutdownGrace = 25 * time.Second
)

// reader accumulates parse errors.
type reader struct {
	env  Getenv
	errs []error
}

func (r *reader) fail(format string, args ...any) {
	r.errs = append(r.errs, fmt.Errorf(format, args...))
}

func (r *reader) err() error { return errors.Join(r.errs...) }

func (r *reader) str(name, def string) string {
	if v := strings.TrimSpace(r.env(name)); v != "" {
		return v
	}
	return def
}

func (r *reader) required(name string) string {
	v := r.str(name, "")
	if v == "" {
		r.fail("%s is required", name)
	}
	return v
}

func (r *reader) duration(name string, def time.Duration) time.Duration {
	v := r.str(name, "")
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		r.fail("%s=%q: want a positive duration such as 30s", name, v)
		return def
	}
	return d
}

func (r *reader) integer(name string, def, minimum int) int {
	v := r.str(name, "")
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < minimum {
		r.fail("%s=%q: want an integer >= %d", name, v, minimum)
		return def
	}
	return n
}

func (r *reader) boolean(name string, def bool) bool {
	v := r.str(name, "")
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		r.fail("%s=%q: want true or false", name, v)
		return def
	}
	return b
}

// secretFile reads the file named by name's value, trimmed of trailing
// newlines; "" when the variable is unset.
func (r *reader) secretFile(name string) string {
	path := r.str(name, "")
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		r.fail("%s: %v", name, err)
		return ""
	}
	v := strings.TrimRight(string(b), "\r\n")
	if v == "" {
		r.fail("%s: %s is empty", name, path)
	}
	return v
}

// requiredSecret is secretFile for a variable that must be set.
func (r *reader) requiredSecret(name string) string {
	if r.str(name, "") == "" {
		r.fail("%s is required", name)
		return ""
	}
	return r.secretFile(name)
}

// jsonFile decodes the file named by name's value into out, refusing
// unknown fields; false when the variable is unset.
func (r *reader) jsonFile(name string, out any) bool {
	path := r.str(name, "")
	if path == "" {
		return false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		r.fail("%s: %v", name, err)
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		r.fail("%s: %s: %v", name, path, err)
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		r.fail("%s: %s: content after the JSON value", name, path)
		return false
	}
	return true
}

// baseURL reads an http(s) base URL without its trailing slash; required
// when def is "".
func (r *reader) baseURL(name, def string) string {
	var v string
	if def == "" {
		v = r.required(name)
	} else {
		v = r.str(name, def)
	}
	if v == "" {
		return ""
	}
	v = strings.TrimRight(v, "/")
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		r.fail("%s=%q: want an http or https URL with a host", name, v)
	}
	return v
}

func (r *reader) common() Common {
	c := Common{AdminAddr: r.str("GORDON_ADMIN_ADDR", DefaultAdminAddr)}
	c.DatabaseURL = r.requiredSecret("GORDON_DATABASE_URL_FILE")
	level, err := logging.ParseLevel(r.str("GORDON_LOG_LEVEL", "info"))
	if err != nil {
		r.fail("GORDON_LOG_LEVEL: %v", err)
	}
	c.LogLevel = level
	return c
}
