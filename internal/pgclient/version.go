package pgclient

import (
	"context"
	"errors"
	"fmt"
)

// ErrNoVersion means the server answered SHOW server_version without a value.
var ErrNoVersion = errors.New("the server reported no server_version")

// ServerVersion connects to the admin database and returns what SHOW
// server_version reports ("18.1 (Debian 18.1-1.pgdg13+1)", for example). It is
// the operator's health probe. A connection failure is returned as
// lifecycle.ErrUnreachable with one of the Err Conn categories as its text and
// is not logged here: a probe repeats every interval, and the cause (which can
// name the host and user, never the password) stays out of the text.
func (a Admin) ServerVersion(ctx context.Context) (string, error) {
	s, err := a.Open(ctx, "")
	if err != nil {
		return "", &connError{category: classify(err), cause: err}
	}
	defer s.Close(ctx)
	rows, err := s.Query(ctx, "SHOW server_version")
	if err != nil {
		return "", fmt.Errorf("read server_version: %w", err)
	}
	if len(rows) == 0 || rows[0]["server_version"] == "" {
		return "", ErrNoVersion
	}
	return rows[0]["server_version"], nil
}
