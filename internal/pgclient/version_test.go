package pgclient

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/craftainer-labs/lib-operator-go/pkg/lifecycle"
)

func TestServerVersion(t *testing.T) {
	ctx := context.Background()
	f := &fakePG{rows: []scripted{{"server_version", []Row{{"server_version": "18.1 (Debian 18.1-1)"}}}}}
	got, err := f.admin().ServerVersion(ctx)
	if err != nil || got != "18.1 (Debian 18.1-1)" {
		t.Fatalf("ServerVersion = %q, %v", got, err)
	}
	f.want(t, "|SHOW server_version")
	if f.closed != 1 {
		t.Errorf("closed %d sessions, want 1", f.closed)
	}
}

func TestServerVersionFailures(t *testing.T) {
	ctx := context.Background()

	// An empty answer and an empty value are not a version.
	for name, rows := range map[string][]Row{"no row": nil, "empty value": {{"server_version": ""}}} {
		f := &fakePG{rows: []scripted{{"server_version", rows}}}
		if _, err := f.admin().ServerVersion(ctx); !errors.Is(err, ErrNoVersion) {
			t.Errorf("%s: %v", name, err)
		}
	}

	// A failed query is an error, and the session is still closed.
	f := &fakePG{failOn: "server_version"}
	if _, err := f.admin().ServerVersion(ctx); !errors.Is(err, errBoom) || f.closed != 1 {
		t.Errorf("failed query: %v (closed %d)", err, f.closed)
	}

	// A connection failure is an unreachable server with a category for its
	// text; neither the cause nor anything it names is in the message.
	cause := errors.New("dial tcp 10.1.2.3:5432: connect: connection refused (password=hunter2)")
	f = &fakePG{openErr: map[string]error{"": cause}}
	_, err := f.admin().ServerVersion(ctx)
	if !errors.Is(err, lifecycle.ErrUnreachable) || !errors.Is(err, ErrConnFailed) || !errors.Is(err, cause) {
		t.Fatalf("connection failure: %v", err)
	}
	if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "10.1.2.3") {
		t.Errorf("the message leaks the cause: %v", err)
	}
}
