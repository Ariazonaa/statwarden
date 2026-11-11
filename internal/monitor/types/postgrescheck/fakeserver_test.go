package postgrescheck

import (
	"encoding/json"
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"
)

// fakeServer is a loopback PostgreSQL wire-protocol endpoint just capable
// enough for livePostgres: it completes startup without authentication,
// answers BEGIN, fails the assertion query with queryErr, and then stops
// answering — so the ROLLBACK that follows is never acknowledged.
type fakeServer struct {
	host string
	port int
}

func startFakeServer(t *testing.T, queryErr *pgproto3.ErrorResponse) fakeServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveFake(conn, queryErr)
		}
	}()
	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	return fakeServer{host: host, port: port}
}

func serveFake(conn net.Conn, queryErr *pgproto3.ErrorResponse) {
	defer conn.Close()
	be := pgproto3.NewBackend(conn, conn)
	if _, err := be.ReceiveStartupMessage(); err != nil {
		return
	}
	be.Send(&pgproto3.AuthenticationOk{})
	be.Send(&pgproto3.ParameterStatus{Name: "server_version", Value: "17.0"})
	be.Send(&pgproto3.BackendKeyData{ProcessID: 1, SecretKey: []byte{0, 0, 0, 1}})
	be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	if err := be.Flush(); err != nil {
		return
	}
	failed := false
	for {
		msg, err := be.Receive()
		if err != nil {
			return
		}
		switch m := msg.(type) {
		case *pgproto3.Query:
			if strings.HasPrefix(strings.ToLower(m.String), "begin") {
				be.Send(&pgproto3.CommandComplete{CommandTag: []byte("BEGIN")})
				be.Send(&pgproto3.ReadyForQuery{TxStatus: 'T'})
				_ = be.Flush()
				continue
			}
			// ROLLBACK, or anything else: never answered.
		case *pgproto3.Parse:
			if !failed {
				failed = true
				be.Send(queryErr)
			}
		case *pgproto3.Sync:
			be.Send(&pgproto3.ReadyForQuery{TxStatus: 'E'})
			_ = be.Flush()
		case *pgproto3.Terminate:
			return
		}
	}
}

func mustCfg(t *testing.T, c Config) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
