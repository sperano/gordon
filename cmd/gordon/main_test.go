package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"serve"}, {"worker", "extra"}} {
		var out, errOut bytes.Buffer
		if code := run(context.Background(), args, func(string) string { return "" }, &out, &errOut); code != exitUsage {
			t.Errorf("run(%v) = %d, want %d", args, code, exitUsage)
		}
		if strings.TrimSpace(errOut.String()) != usage {
			t.Errorf("run(%v) printed %q", args, errOut.String())
		}
	}
}

func TestConfigErrorExitsWithItsReason(t *testing.T) {
	for _, mode := range []string{"receiver", "worker", "migrate"} {
		var out bytes.Buffer
		code := run(context.Background(), []string{mode}, func(string) string { return "" }, &out, &out)
		if code != exitFailed || !strings.Contains(out.String(), "GORDON_DATABASE_URL_FILE is required") ||
			!strings.Contains(out.String(), `"mode":"`+mode+`"`) {
			t.Errorf("%s with no configuration: exit %d, log %s", mode, code, out.String())
		}
	}
}
